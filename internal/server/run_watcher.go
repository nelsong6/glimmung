// Package server — run_watcher.go owns the cluster-wide k8s Watch
// that drives glimmung's terminal-Job detection.
//
// This replaces the periodic-reconciler-as-primary shape with an
// event-driven primary: the apiserver pushes Job MODIFIED events
// to glimmung as kubelet stamps Complete=True / Failed=True conditions
// (typically <200ms after the pod transitions), and glimmung dispatches
// into the existing synthesis paths immediately. The reconciler in
// run_execution_reconciler.go drops to a relaxed cadence (1h) as
// belt-and-braces for missed events.
//
// Architectural reasoning lives in docs/observability.md "Why a Watch,
// not a poll" — short version: a polling reconciler in an otherwise
// event-driven system is a settled-contract mismatch, and the answer
// to "why did this transition fire at time T" should be "the apiserver
// pushed event UID=X at T-200ms," not "the 30s tick happened to hit."
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nelsong6/glimmung/internal/metrics"
)

const (
	// watchManagedByOuter is the label value glimmung's native
	// launcher sets on every phase Job (see nativeJobManifest).
	watchManagedByOuter = "glimmung"
	// watchManagedByInner is the label value the ambience inner-agent
	// Job manifest sets so child Jobs in slot namespaces fall under
	// the same cluster-wide Watch.
	watchManagedByInner = "glimmung-inner"

	// watchOuterSelector scopes one of the two Watch goroutines to
	// glimmung's own phase Jobs in glimmung-runs. The native-job=true
	// label is set by nativeJobManifest on every phase Job and is the
	// signal that distinguishes them from test-slot installer Jobs
	// (which also carry managed-by=glimmung but aren't run-bound).
	watchOuterSelector = "app.kubernetes.io/managed-by=" + watchManagedByOuter +
		",glimmung.romaine.life/native-job=true"

	// watchInnerSelector scopes the second Watch to inner agent Jobs
	// ambience labels managed-by=glimmung-inner. They live in slot
	// namespaces; the apiserver returns them on a cluster-wide Watch.
	watchInnerSelector = "app.kubernetes.io/managed-by=" + watchManagedByInner

	// watchInitialBackoff / watchMaxBackoff bound the reconnect cadence
	// after a transport error or a Gone response. The first reconnect
	// retries quickly to ride out an apiserver rolling restart; sustained
	// failure escalates to a longer wait so we stop hammering an
	// unhealthy control plane.
	watchInitialBackoff = 1 * time.Second
	watchMaxBackoff     = 30 * time.Second

	// watchTimeoutSeconds is the apiserver-side timeoutSeconds we
	// request on each Watch. The apiserver will close the connection
	// after this window; the watcher loops back into a fresh Watch
	// with the latest resourceVersion. 10 minutes is the kube-apiserver
	// default and keeps connection-level state bounded.
	watchTimeoutSeconds = 600
)

// StartNativeJobWatcher launches the cluster-wide Watch goroutine.
// Returns immediately; the goroutine runs until ctx is cancelled. No
// state is exposed: the watcher's only side effect is to dispatch
// terminal Job events into the synthesis paths the reconciler also
// uses, plus update the watch-health metrics.
//
// When the launcher does not implement NativeJobStatusGetter the
// watcher is a no-op — the same missing-capability gate the reconciler
// uses. This keeps unit tests with the fake launcher from spawning a
// live HTTP connection attempt.
func StartNativeJobWatcher(ctx context.Context, settings Settings, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	timeoutStore, _ := store.(RunDispatchTimeoutStore)
	if timeoutStore == nil {
		return
	}
	jobStatusGetter, _ := nativeLauncher.(NativeJobStatusGetter)
	if jobStatusGetter == nil {
		return
	}
	completionStore, _ := any(timeoutStore).(RunCompletionStore)
	jobStore, _ := any(timeoutStore).(NativeJobCompletionStore)
	eventStore, _ := any(timeoutStore).(NativeRunStore)
	if completionStore == nil || jobStore == nil || eventStore == nil {
		return
	}
	logsFetcher, _ := nativeLauncher.(NativeJobLogsFetcher)
	urlBuilder := settingsLogArchiveURLBuilder{settings: settings}

	common := watcherDeps{
		settings:        settings,
		store:           timeoutStore,
		completionStore: completionStore,
		jobStore:        jobStore,
		eventStore:      eventStore,
		nativeLauncher:  nativeLauncher,
		statusGetter:    jobStatusGetter,
		logsFetcher:     logsFetcher,
		urlBuilder:      urlBuilder,
		namespace:       strings.TrimSpace(settings.NativeRunnerNamespace),
		logf:            logf,
	}
	go (&k8sJobWatcher{watcherDeps: common, labelSelector: watchOuterSelector}).run(ctx)
	go (&k8sJobWatcher{watcherDeps: common, labelSelector: watchInnerSelector}).run(ctx)
}

// watcherDeps groups the common dependencies shared by both Watch
// goroutines so neither carries a duplicated 11-field constructor.
type watcherDeps struct {
	settings        Settings
	store           RunDispatchTimeoutStore
	completionStore RunCompletionStore
	jobStore        NativeJobCompletionStore
	eventStore      NativeRunStore
	nativeLauncher  NativeLauncher
	statusGetter    NativeJobStatusGetter
	logsFetcher     NativeJobLogsFetcher
	urlBuilder      LogArchiveURLBuilder
	namespace       string
	logf            func(string, ...any)
}

// k8sJobWatcher is one of the two production Watch loops (one per
// labelSelector). The shared deps come from watcherDeps; labelSelector
// distinguishes which kind of Job this goroutine sees.
type k8sJobWatcher struct {
	watcherDeps
	labelSelector string

	// lastEventAt is updated atomically every time the Watch
	// successfully receives an event (any kind, including BOOKMARK).
	// The disconnect-seconds gauge is derived from this.
	lastEventAt atomic.Int64
}

func (w *k8sJobWatcher) log(format string, args ...any) {
	if w.logf != nil {
		w.logf(format, args...)
	}
}

// run is the watcher's lifetime loop. List + Watch + reconnect, with
// metric updates on every iteration boundary.
func (w *k8sJobWatcher) run(ctx context.Context) {
	w.lastEventAt.Store(time.Now().UnixNano())
	backoff := watchInitialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		resourceVersion, err := w.listAndSync(ctx)
		if err != nil {
			metrics.RecordRunWatchEvent("control", "list_error")
			w.log("native-job watcher list failed: %v", err)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		// Successful list resets backoff.
		backoff = watchInitialBackoff
		w.lastEventAt.Store(time.Now().UnixNano())
		metrics.SetRunWatchDisconnectedSeconds(0)
		if err := w.watch(ctx, resourceVersion); err != nil {
			if errors.Is(err, errWatchGone) {
				// 410 — re-list and resume.
				metrics.RecordRunWatchEvent("control", "resource_version_gone")
				continue
			}
			if errors.Is(err, errWatchClosedNormally) {
				// apiserver hit its timeoutSeconds; loop back and
				// resume with the same resourceVersion.
				continue
			}
			if ctx.Err() != nil {
				return
			}
			metrics.RecordRunWatchEvent("control", "stream_error")
			w.log("native-job watcher stream ended: %v", err)
			// Stale resourceVersion may have caused this; the next
			// listAndSync will fetch a fresh one.
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
	}
}

// LastEventAt is exposed for tests + the metric refresher.
func (w *k8sJobWatcher) LastEventAt() time.Time {
	ns := w.lastEventAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// listAndSync calls GET on the cluster-wide Jobs collection (matching
// the watch's labelSelector), processes any Jobs that already have a
// terminal condition, and returns the list's resourceVersion so the
// caller can Watch from there without missing or duplicating events.
//
// k8s guarantees that a Watch started with the LIST's resourceVersion
// receives every event that has happened since that snapshot, in order.
// This is the standard "list + watch" pattern.
func (w *k8sJobWatcher) listAndSync(ctx context.Context) (string, error) {
	body, _, err := w.getJSON(ctx, w.listPath())
	if err != nil {
		return "", err
	}
	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("decode jobs list: %w", err)
	}
	for _, item := range list.Items {
		w.dispatchJobObject(ctx, item, "list_sync", "")
	}
	return list.Metadata.ResourceVersion, nil
}

// watch consumes the streaming Watch endpoint at the supplied
// resourceVersion. Returns nil on apiserver-driven graceful close
// (timeoutSeconds elapsed); the run() loop then loops back without
// re-listing. Returns errWatchGone when the apiserver tells us our
// resourceVersion is stale (410); errWatchClosedNormally on graceful
// timeoutSeconds elapsed; any other error otherwise.
func (w *k8sJobWatcher) watch(ctx context.Context, resourceVersion string) error {
	path := w.watchPath(resourceVersion)
	req, err := w.newAuthedRequest(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	client := w.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return errWatchGone
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("watch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Stream line-delimited JSON. scanner.Buffer is bumped because a
	// single Job object can exceed the default 64KiB when its events
	// list is long.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev watchEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			metrics.RecordRunWatchEvent("control", "decode_error")
			w.log("native-job watch decode failed: %v", err)
			continue
		}
		w.lastEventAt.Store(time.Now().UnixNano())
		w.handleEvent(ctx, ev)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errWatchClosedNormally
}

// watchEvent is the apiserver's streamed event envelope.
type watchEvent struct {
	Type   string         `json:"type"`
	Object map[string]any `json:"object"`
}

func (w *k8sJobWatcher) handleEvent(ctx context.Context, ev watchEvent) {
	switch ev.Type {
	case "BOOKMARK":
		// apiserver heartbeat carrying an updated resourceVersion;
		// no-op for us beyond the lastEventAt refresh above. We don't
		// store the resourceVersion separately because every watch
		// iteration re-derives it from the next list.
		metrics.RecordRunWatchEvent("control", "bookmark")
		return
	case "ADDED", "MODIFIED":
		w.dispatchJobObject(ctx, ev.Object, strings.ToLower(ev.Type), kind(ev.Object))
	case "DELETED":
		// Inner-Job TTL'd / outer Job manually deleted. The
		// reconciler used to fire pod_gone via the not-found GET
		// path; here we just record the event and rely on the
		// terminal condition (which fired on MODIFIED before the
		// DELETED) to have already been synthesized. If we never
		// observed a terminal MODIFIED (rare — apiserver lost the
		// event window) the next list_sync catches it.
		metrics.RecordRunWatchEvent(kind(ev.Object), "deleted")
	case "ERROR":
		metrics.RecordRunWatchEvent("control", "error")
		// "ERROR" objects carry an apiserver Status. Specifically a
		// 410 Gone here is the canonical signal that the
		// resourceVersion is stale. The simplest correct response is
		// to terminate the current watch so run() re-lists.
		w.log("native-job watch ERROR event payload=%v", ev.Object)
	default:
		metrics.RecordRunWatchEvent("control", "unknown_type")
	}
}

// dispatchJobObject inspects one batch/v1.Job and, if it has reached
// a terminal condition, dispatches into the synthesis path. action
// labels the metric ("list_sync" / "added" / "modified"); kind is
// "outer" or "inner".
func (w *k8sJobWatcher) dispatchJobObject(ctx context.Context, job map[string]any, action, kindHint string) {
	jobKind := kindHint
	if jobKind == "" {
		jobKind = kind(job)
	}
	metrics.RecordRunWatchEvent(jobKind, action)
	status := parseNativeJobStatus(job)
	if !status.IsTerminallySucceeded() && !status.IsTerminallyFailed() {
		return
	}
	switch jobKind {
	case "outer":
		w.dispatchOuterTerminal(ctx, job, status, action)
	case "inner":
		w.dispatchInnerTerminal(ctx, job, status, action)
	}
}

// dispatchOuterTerminal synthesizes a phase-job completion for an
// outer Job that has reached a terminal k8s condition. The labels
// glimmung's launcher set carry the full run / phase / job-id triple
// so we can call straight into RecordNativeJobCompletion without a
// database lookup.
func (w *k8sJobWatcher) dispatchOuterTerminal(ctx context.Context, job map[string]any, status NativeJobStatus, action string) {
	labels := jobLabels(job)
	project := strings.TrimSpace(labels["glimmung.romaine.life/project"])
	runRef := strings.TrimSpace(labels["glimmung.romaine.life/run-ref"])
	phase := strings.TrimSpace(labels["glimmung.romaine.life/phase"])
	jobID := strings.TrimSpace(labels["glimmung.romaine.life/job-id"])
	if project == "" || runRef == "" || phase == "" || jobID == "" {
		w.log("native-job watch outer event missing labels: %v", labels)
		return
	}
	runID := w.findRunIDByRunRef(ctx, project, runRef)
	if runID == "" {
		// Run not in our in-progress set — already terminated or
		// belongs to a project we don't manage. Nothing to do.
		return
	}
	k8sJobName := jobObjectName(job)
	conclusion, terminalReason, summary := deriveTerminalFromStatus(status, k8sJobName)
	logURL := captureOuterJobLogs(ctx, w.logsFetcher, currentRunReconcilerArtifactWriter(), project, runID, w.namespace, k8sJobName, w.logf)
	if logURL == "" && w.urlBuilder != nil {
		logURL = w.urlBuilder.BuildLogArchiveURL(w.namespace, k8sJobName, time.Time{}, time.Now().UTC())
	}
	payload := CompletionPayload{
		JobID:           &jobID,
		Conclusion:      conclusion,
		SummaryMarkdown: &summary,
		TerminalReason:  terminalReason,
		LogArchiveURL:   logURL,
	}
	result, err := w.jobStore.RecordNativeJobCompletion(ctx, project, runID, payload)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		// Already completed (runner callback won the race, or the
		// reconciler synthesized this) — idempotent success.
		return
	}
	if err != nil {
		w.log("native-job watch outer synthesis failed run=%s job=%s: %v", runID, jobID, err)
		return
	}
	metrics.RecordRunPhaseJobTerminal(conclusion, NormalizeJobTerminalReason(terminalReason))
	metrics.RecordRunWatchEvent("outer", "synthesized_"+action)
	if result.CompletionReady {
		if _, err := processSyntheticRunCompletion(ctx, w.completionStore, w.nativeLauncher, project, runID, result.PhasePayload); err != nil {
			w.log("native-job watch outer phase completion failed run=%s: %v", runID, err)
		}
	}
}

// dispatchInnerTerminal emits an inner_job_terminated event for an
// inner Job that has reached a terminal k8s condition. Mirrors the
// existing reconciler watcher path but is triggered by the apiserver
// push, not the 1h fallback tick.
func (w *k8sJobWatcher) dispatchInnerTerminal(ctx context.Context, job map[string]any, status NativeJobStatus, action string) {
	namespace := jobObjectNamespace(job)
	jobName := jobObjectName(job)
	if namespace == "" || jobName == "" {
		return
	}
	project, runID, parentJobID, parentStepSlug, phaseName, registeredAt, ok := w.findInnerJobOwner(ctx, namespace, jobName)
	if !ok {
		// We haven't seen the registration event yet (runner is
		// racing the kubelet on a fast-completing Job). The 1h
		// reconciler will catch it; or, more likely, the
		// registration event will arrive shortly and the next
		// MODIFIED will trigger us again.
		return
	}
	var state, reason, completedAt string
	if status.IsTerminallySucceeded() && !status.IsTerminallyFailed() {
		state = "succeeded"
	} else {
		state = "failed"
		reason = mapK8sFailedReasonToInnerJobReason(status.FailureReason())
	}
	completedAt = formatTerminalTime(status.TerminalTime(), time.Now().UTC())

	ij := InnerJobRef{
		ParentJobID:    parentJobID,
		ParentStepSlug: stringPtrOrNil(parentStepSlug),
		Namespace:      namespace,
		JobName:        jobName,
		RegisteredAt:   registeredAt,
	}
	logURL := captureInnerJobLogs(ctx, w.logsFetcher, currentRunReconcilerArtifactWriter(), project, runID, namespace, jobName, w.logf)
	if logURL == "" && w.urlBuilder != nil {
		from := parseInnerJobRegisteredAt(registeredAt)
		toTime := time.Now().UTC()
		if t, err := time.Parse(time.RFC3339Nano, completedAt); err == nil {
			toTime = t
		}
		logURL = w.urlBuilder.BuildLogArchiveURL(namespace, jobName, from, toTime)
	}
	metadata := map[string]any{
		"namespace":    namespace,
		"job_name":     jobName,
		"state":        state,
		"reason":       reason,
		"completed_at": completedAt,
		"phase":        phaseName,
	}
	if logURL != "" {
		metadata["log_archive_url"] = logURL
	}
	req := NativeRunEventRequest{
		JobID:    parentJobID,
		Seq:      innerJobTerminationSeq(ij),
		Event:    "inner_job_terminated",
		StepSlug: stringPtrOrNil(parentStepSlug),
		Metadata: metadata,
	}
	if _, err := w.eventStore.RecordNativeEventByID(ctx, project, runID, req); err != nil {
		if errors.Is(err, ErrConflict) {
			return
		}
		w.log("native-job watch inner termination submit failed run=%s job=%s/%s: %v", runID, namespace, jobName, err)
		return
	}
	metrics.RecordRunWatchEvent("inner", "synthesized_"+action)
}

// findRunIDByRunRef resolves a run-ref label back to a run ID by
// scanning the project's in-progress runs. Cheap because the label
// gates by project first.
func (w *k8sJobWatcher) findRunIDByRunRef(ctx context.Context, project, runRef string) string {
	runs, err := w.store.ListProjectRuns(ctx, project, 500)
	if err != nil {
		w.log("native-job watch list runs failed project=%s: %v", project, err)
		return ""
	}
	for _, run := range runs {
		if run.State != "in_progress" {
			continue
		}
		// The Job's label uses the colon-separated run ref format
		// (project#issue/runs/X.Y) but launcher.labelValue collapses
		// characters; compare normalised.
		if labelValue(run.RunRef) == runRef && run.ID != "" {
			return run.ID
		}
	}
	return ""
}

// findInnerJobOwner walks in-progress runs across all projects to
// find which one has an inner_jobs[] entry matching this k8s
// namespace+jobName. Inner Jobs do not carry glimmung's per-run
// labels because ambience owns the manifest; we rely on the
// previously-emitted inner_job_registered event to ground the lookup.
func (w *k8sJobWatcher) findInnerJobOwner(ctx context.Context, namespace, jobName string) (project, runID, parentJobID, parentStepSlug, phaseName, registeredAt string, ok bool) {
	projects, err := w.store.ListProjects(ctx)
	if err != nil {
		return "", "", "", "", "", "", false
	}
	for _, p := range projects {
		name := firstNonEmpty(p.Name, p.ID)
		if name == "" {
			continue
		}
		runs, err := w.store.ListProjectRuns(ctx, name, 500)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if run.State != "in_progress" {
				continue
			}
			for _, phase := range run.PhaseExecutions {
				for _, ij := range phase.InnerJobs {
					if ij.Namespace == namespace && ij.JobName == jobName {
						step := ""
						if ij.ParentStepSlug != nil {
							step = *ij.ParentStepSlug
						}
						return name, run.ID, ij.ParentJobID, step, phase.Name, ij.RegisteredAt, true
					}
				}
			}
		}
	}
	return "", "", "", "", "", "", false
}

// listPath / watchPath are split for testability.
func (w *k8sJobWatcher) listPath() string {
	q := url.Values{}
	q.Set("labelSelector", w.labelSelector)
	return "/apis/batch/v1/jobs?" + q.Encode()
}

func (w *k8sJobWatcher) watchPath(resourceVersion string) string {
	q := url.Values{}
	q.Set("watch", "true")
	q.Set("labelSelector", w.labelSelector)
	q.Set("resourceVersion", resourceVersion)
	q.Set("allowWatchBookmarks", "true")
	q.Set("timeoutSeconds", fmt.Sprintf("%d", watchTimeoutSeconds))
	return "/apis/batch/v1/jobs?" + q.Encode()
}

// getJSON does a one-shot authenticated GET and returns the body.
func (w *k8sJobWatcher) getJSON(ctx context.Context, path string) ([]byte, int, error) {
	req, err := w.newAuthedRequest(ctx, http.MethodGet, path)
	if err != nil {
		return nil, 0, err
	}
	resp, err := w.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, resp.StatusCode, fmt.Errorf("k8s GET %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func (w *k8sJobWatcher) newAuthedRequest(ctx context.Context, method, path string) (*http.Request, error) {
	full := strings.TrimRight(w.settings.K8sAPIHost, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(w.settings.K8sSATokenPath)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (w *k8sJobWatcher) httpClient() *http.Client {
	// Watch needs a long timeout because the apiserver streams events
	// for up to timeoutSeconds. List uses the same client; the LIST
	// itself is small so this doesn't penalise it.
	return &http.Client{
		// No Timeout — the Watch is server-bounded by
		// timeoutSeconds. Cancellation is via ctx on the request.
		Transport: (&KubernetesNativeLauncher{Settings: w.settings}).transport(),
	}
}

// ---- helpers ----

// deriveTerminalFromStatus mirrors evaluateActiveJobFailure's
// status-to-conclusion mapping. We can't reuse evaluateActiveJobFailure
// directly because it also handles the not-found and grace-period
// branches that don't apply to a freshly-pushed Watch event.
func deriveTerminalFromStatus(status NativeJobStatus, k8sJobName string) (conclusion, terminalReason, summary string) {
	if status.IsTerminallySucceeded() && !status.IsTerminallyFailed() {
		// Watch saw Complete=True but the runner never pushed
		// /completed — surface as failed/callback_lost so evidence
		// fields stay accurate (runner-side has the verification
		// payload; we don't).
		return "failed", JobTerminalReasonCallbackLost,
			fmt.Sprintf("native job %q completed in kubernetes but its completion callback was never received", k8sJobName)
	}
	reason := status.FailureReason()
	message := status.FailureMessage()
	conclusion = "failed"
	terminalReason = JobTerminalReasonJobFailed
	switch reason {
	case "DeadlineExceeded":
		conclusion = "timed_out"
		terminalReason = JobTerminalReasonDeadlineExceeded
	case "BackoffLimitExceeded":
		conclusion = "timed_out"
		terminalReason = JobTerminalReasonBackoffExceeded
	}
	summary = fmt.Sprintf("native job %q ended with kubernetes condition Failed=true reason=%q: %s", k8sJobName, reason, strings.TrimSpace(message))
	return conclusion, terminalReason, summary
}

// captureOuterJobLogs is the outer-Job counterpart of
// captureInnerJobLogs (which lives in run_execution_reconciler.go).
// Same artifact path layout as inner Jobs except scoped to the parent
// Job's k8s name rather than (namespace, jobName).
func captureOuterJobLogs(ctx context.Context, fetcher NativeJobLogsFetcher, writer ArtifactWriter, project, runID, namespace, jobName string, logf func(string, ...any)) string {
	if fetcher == nil || writer == nil {
		return ""
	}
	project = strings.TrimSpace(project)
	runID = strings.TrimSpace(runID)
	namespace = strings.TrimSpace(namespace)
	jobName = strings.TrimSpace(jobName)
	if project == "" || runID == "" || namespace == "" || jobName == "" {
		return ""
	}
	body, err := fetcher.GetNativeJobLogs(ctx, namespace, jobName, 0)
	if err != nil {
		if logf != nil {
			logf("outer-job log capture fetch failed namespace=%s job=%s: %v", namespace, jobName, err)
		}
		return ""
	}
	if len(body) == 0 {
		return ""
	}
	blobPath := "runs/" + url.PathEscape(project) + "/" + url.PathEscape(runID) + "/phase_jobs/" + url.PathEscape(jobName) + ".log"
	if _, err := writer.Upload(ctx, blobPath, body, "text/plain; charset=utf-8"); err != nil {
		if logf != nil {
			logf("outer-job log capture upload failed namespace=%s job=%s: %v", namespace, jobName, err)
		}
		return ""
	}
	return "/v1/artifacts/" + blobPath
}

// kind classifies a Job by its managed-by label. The Watch label
// selector means anything we receive matches one of these two values;
// unknown returns "control" to keep metric labels bounded.
func kind(job map[string]any) string {
	switch jobLabel(job, "app.kubernetes.io/managed-by") {
	case watchManagedByOuter:
		return "outer"
	case watchManagedByInner:
		return "inner"
	default:
		return "control"
	}
}

func jobLabels(job map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range labelsFromMeta(job) {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func labelsFromMeta(job map[string]any) map[string]any {
	meta, _ := job["metadata"].(map[string]any)
	if meta == nil {
		return nil
	}
	labels, _ := meta["labels"].(map[string]any)
	return labels
}

func jobLabel(job map[string]any, key string) string {
	labels := labelsFromMeta(job)
	if labels == nil {
		return ""
	}
	v, _ := labels[key].(string)
	return v
}

func jobObjectName(job map[string]any) string {
	meta, _ := job["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	if s, ok := meta["name"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func jobObjectNamespace(job map[string]any) string {
	meta, _ := job["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	if s, ok := meta["namespace"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func nextBackoff(prev time.Duration) time.Duration {
	next := prev * 2
	if next > watchMaxBackoff {
		next = watchMaxBackoff
	}
	return next
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func stringPtrOrNil(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// errWatchGone is returned by watch() on an apiserver 410 Gone — the
// caller should re-list to get a fresh resourceVersion.
var errWatchGone = errors.New("watch resourceVersion gone")

// errWatchClosedNormally is returned by watch() when the apiserver
// gracefully closes the connection after timeoutSeconds. The caller
// should reconnect with the same (or a newer) resourceVersion.
var errWatchClosedNormally = errors.New("watch closed by apiserver")
