package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nelsong6/glimmung/internal/metrics"
)

const defaultRunDispatchTimeoutSeconds = 600

type RunDispatchTimeoutStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListProjectRuns(ctx context.Context, project string, limit int) ([]RunReport, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

// activeJobFailureGracePeriod gives the native runner a small window after
// k8s marks a Job terminally Failed before the reconciler synthesizes a
// completion. The completion callback may already be in flight; without a
// grace period we race and double-complete. 60s is well inside the runner's
// http retry budget.
const activeJobFailureGracePeriod = 60 * time.Second

// reconcilerCadence is the periodic tick interval of the reconciler
// after the cluster-wide Watch took over as primary detection. 1h is
// far above any normal reconnect-and-recover window for the Watch but
// short enough to keep observability budgets predictable. Operators
// who want faster recovery should fix the Watch path rather than tune
// this knob.
const reconcilerCadence = 1 * time.Hour

// LogArchiveURLBuilder mints a Grafana Explore URL pointing at the
// cluster Loki datasource for the given pod + time window. The
// reconciler invokes it when synthesizing a terminal completion so the
// dashboard's log-archive surface has a working durable pointer to the
// child's logs (within Loki retention).
type LogArchiveURLBuilder interface {
	BuildLogArchiveURL(namespace, k8sJobName string, from, to time.Time) string
}

// settingsLogArchiveURLBuilder is the production implementation that
// reads Settings.GrafanaBaseURL / Settings.GrafanaLokiDatasource. When
// either is empty (misconfigured environment) the builder returns the
// empty string so the dashboard renders "unavailable" instead of a
// broken link.
type settingsLogArchiveURLBuilder struct{ settings Settings }

func (b settingsLogArchiveURLBuilder) BuildLogArchiveURL(namespace, k8sJobName string, from, to time.Time) string {
	base := strings.TrimRight(strings.TrimSpace(b.settings.GrafanaBaseURL), "/")
	datasource := strings.TrimSpace(b.settings.GrafanaLokiDatasource)
	if base == "" || namespace == "" || k8sJobName == "" {
		return ""
	}
	if datasource == "" {
		datasource = "loki"
	}
	expr := fmt.Sprintf(`{namespace=%q,pod=~%q}`, namespace, escapeRegexLiteral(k8sJobName)+"-.*")
	left := map[string]any{
		"datasource": datasource,
		"queries": []map[string]any{{
			"refId":      "A",
			"datasource": map[string]any{"type": "loki", "uid": datasource},
			"expr":       expr,
			"queryType":  "range",
		}},
		"range": map[string]any{
			"from": grafanaRangeBound(from, "now-24h"),
			"to":   grafanaRangeBound(to, "now"),
		},
	}
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return ""
	}
	params := url.Values{}
	params.Set("orgId", "1")
	params.Set("left", string(leftJSON))
	return base + "/explore?" + params.Encode()
}

// escapeRegexLiteral escapes the characters that could appear in a
// kubernetes job name (DNS labels — alphanumerics + '-'), mirroring
// the equivalent TS helper. The escape stays safe if the DNS-label
// constraint ever loosens upstream.
func escapeRegexLiteral(value string) string {
	const meta = `\.+*?()|[]{}^$`
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if strings.ContainsRune(meta, rune(ch)) {
			out.WriteByte('\\')
		}
		out.WriteByte(ch)
	}
	return out.String()
}

// grafanaRangeBound converts a time.Time to Grafana's Explore range
// encoding (epoch milliseconds as a string). Zero times fall through
// to the fallback (a "now"-relative string).
func grafanaRangeBound(t time.Time, fallback string) string {
	if t.IsZero() {
		return fallback
	}
	return strconv.FormatInt(t.UTC().UnixMilli(), 10)
}

// runReconcilerArtifactRegistry holds the artifact writer the
// reconciler delegates to for durable log capture (inner-Job Stage 3).
// Mirrors the inspection-sweep registry shape so the wiring code in
// server.go has one consistent pattern for cross-cutting hooks that
// reach background goroutines.
var runReconcilerArtifactRegistry struct {
	mu     sync.RWMutex
	writer ArtifactWriter
}

// SetRunReconcilerArtifactWriter registers the artifact writer the
// run-execution reconciler uses to capture inner-Job pod logs on
// terminal transitions. Called once at handler construction; safe to
// call again with the same value during tests. Passing nil disables
// log capture; the inner-Job watcher then falls back to the Grafana
// Loki deep-link as its log_archive_url.
func SetRunReconcilerArtifactWriter(writer ArtifactWriter) {
	runReconcilerArtifactRegistry.mu.Lock()
	runReconcilerArtifactRegistry.writer = writer
	runReconcilerArtifactRegistry.mu.Unlock()
}

func currentRunReconcilerArtifactWriter() ArtifactWriter {
	runReconcilerArtifactRegistry.mu.RLock()
	defer runReconcilerArtifactRegistry.mu.RUnlock()
	return runReconcilerArtifactRegistry.writer
}

// captureInnerJobLogs fetches the inner Job's pod stdout (bounded),
// uploads it to the artifact store under
// runs/{project}/{runID}/inner_jobs/{namespace}/{jobName}.log, and
// returns the /v1/artifacts/... URL the run-report UI dereferences.
// Empty return means the caller should keep the Grafana Loki fallback
// link — either because the artifact writer is unconfigured, the
// launcher does not implement NativeJobLogsFetcher, the fetch
// returned no bytes (TTL'd pod), or the upload failed transiently.
// Errors from the upload path are logged via logf but not returned;
// the caller does not block the watcher tick on capture failures.
func captureInnerJobLogs(ctx context.Context, fetcher NativeJobLogsFetcher, writer ArtifactWriter, project, runID, namespace, jobName string, logf func(string, ...any)) string {
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
			logf("inner-job log capture fetch failed namespace=%s job=%s: %v", namespace, jobName, err)
		}
		return ""
	}
	if len(body) == 0 {
		return ""
	}
	blobPath := innerJobLogBlobPath(project, runID, namespace, jobName)
	if _, err := writer.Upload(ctx, blobPath, body, "text/plain; charset=utf-8"); err != nil {
		if logf != nil {
			logf("inner-job log capture upload failed namespace=%s job=%s: %v", namespace, jobName, err)
		}
		return ""
	}
	return "/v1/artifacts/" + blobPath
}

// innerJobLogBlobPath derives the canonical artifact blob path for an
// inner-Job log capture. The runs/{project}/{runID}/ prefix matches
// the existing inspection artifact layout and is allowlisted by the
// artifact-download route's prefix validator.
func innerJobLogBlobPath(project, runID, namespace, jobName string) string {
	return "runs/" + url.PathEscape(project) + "/" + url.PathEscape(runID) +
		"/inner_jobs/" + url.PathEscape(namespace) + "/" + url.PathEscape(jobName) + ".log"
}

func StartRunDispatchTimeoutReconciler(ctx context.Context, settings Settings, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	timeout := time.Duration(settings.NativeRunnerDispatchTimeoutSeconds) * time.Second
	timeoutStore, _ := store.(RunDispatchTimeoutStore)
	jobStatusGetter, _ := nativeLauncher.(NativeJobStatusGetter)
	logsFetcher, _ := nativeLauncher.(NativeJobLogsFetcher)
	if timeout <= 0 && jobStatusGetter == nil {
		return
	}
	if timeoutStore == nil {
		return
	}
	namespace := strings.TrimSpace(settings.NativeRunnerNamespace)
	urlBuilder := settingsLogArchiveURLBuilder{settings: settings}
	go func() {
		// Reconciler cadence: 1h. After the cluster-wide k8s Watch
		// (run_watcher.go) became primary detection, this reconciler
		// drops to belt-and-braces. Sustained non-zero
		// glimmung_run_reconciler_caught_total is the alert signal
		// that the Watch path is broken.
		ticker := time.NewTicker(reconcilerCadence)
		defer ticker.Stop()
		for {
			if timeout > 0 {
				expired, err := ExpireRunDispatchTimeouts(ctx, timeoutStore, nativeLauncher, timeout, time.Now().UTC())
				if err != nil && logf != nil {
					logf("run dispatch-timeout reconcile failed: %v", err)
				}
				if expired > 0 && logf != nil {
					logf("run dispatch-timeout reconciled expired=%d", expired)
				}
			}
			if jobStatusGetter != nil && namespace != "" {
				completed, err := ExpireFailedActiveJobs(ctx, timeoutStore, nativeLauncher, jobStatusGetter, namespace, urlBuilder, activeJobFailureGracePeriod, time.Now().UTC())
				if err != nil && logf != nil {
					logf("run active-job failure reconcile failed: %v", err)
				}
				if completed > 0 && logf != nil {
					logf("run active-job failure reconciled completed=%d", completed)
				}
			}
			if jobStatusGetter != nil {
				artifactWriter := currentRunReconcilerArtifactWriter()
				emitted, err := ExpireInnerJobTerminations(ctx, timeoutStore, jobStatusGetter, logsFetcher, artifactWriter, urlBuilder, activeJobFailureGracePeriod, time.Now().UTC(), logf)
				if err != nil && logf != nil {
					logf("inner-job termination reconcile failed: %v", err)
				}
				if emitted > 0 && logf != nil {
					logf("inner-job termination reconciled emitted=%d", emitted)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func ExpireRunDispatchTimeouts(ctx context.Context, store RunDispatchTimeoutStore, nativeLauncher NativeLauncher, timeout time.Duration, now time.Time) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, project := range projects {
		name := firstNonEmpty(project.Name, project.ID)
		if name == "" {
			continue
		}
		runs, err := store.ListProjectRuns(ctx, name, 500)
		if err != nil {
			return expired, err
		}
		for _, run := range runs {
			if run.State != "in_progress" || run.ID == "" {
				continue
			}
			phase, ok := dispatchTimedOutPhase(run, timeout, now)
			if !ok {
				continue
			}
			completed, err := completeDispatchTimedOutPhase(ctx, store, nativeLauncher, run, phase, timeout)
			if err != nil {
				return expired, err
			}
			if !completed {
				if _, err := store.AbortRunByID(ctx, run.Project, run.ID, "dispatch_timeout"); err != nil {
					return expired, err
				}
			}
			expired++
		}
	}
	return expired, nil
}

func completeDispatchTimedOutPhase(ctx context.Context, store RunDispatchTimeoutStore, nativeLauncher NativeLauncher, run RunReport, phaseName string, timeout time.Duration) (bool, error) {
	completionStore, ok := any(store).(RunCompletionStore)
	if !ok || completionStore == nil {
		return false, nil
	}
	jobStore, ok := any(store).(NativeJobCompletionStore)
	if !ok || jobStore == nil || nativeLauncher == nil {
		return false, nil
	}
	jobIDs := timedOutJobIDs(run, phaseName)
	if len(jobIDs) == 0 {
		return false, nil
	}
	summary := fmt.Sprintf("native phase %q exceeded dispatch timeout after %s", phaseName, timeout)
	var ready *NativeJobCompletionResult
	for _, id := range jobIDs {
		jobID := id
		payload := CompletionPayload{
			JobID:           &jobID,
			Conclusion:      "timed_out",
			SummaryMarkdown: &summary,
		}
		result, err := jobStore.RecordNativeJobCompletion(ctx, run.Project, run.ID, payload)
		if err != nil {
			return false, err
		}
		if result.CompletionReady {
			ready = &result
		}
	}
	if ready == nil {
		return true, nil
	}
	_, err := processSyntheticRunCompletion(ctx, completionStore, nativeLauncher, run.Project, run.ID, ready.PhasePayload)
	return true, err
}

func timedOutJobIDs(run RunReport, phaseName string) []string {
	for _, phase := range run.PhaseExecutions {
		if phase.Name != phaseName {
			continue
		}
		ids := make([]string, 0, len(phase.Jobs))
		for _, job := range phase.Jobs {
			switch job.State {
			case "", "not_started", "dispatching", "active":
				if job.ID != "" {
					ids = append(ids, job.ID)
				}
			}
		}
		return ids
	}
	return nil
}

func processSyntheticRunCompletion(ctx context.Context, store RunCompletionStore, nativeLauncher NativeLauncher, project, runID string, payload CompletionPayload) (*RunCallbackResult, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	w := &captureResponseWriter{header: http.Header{}}
	result := processRunCompletion(ctx, w, req, store, nativeLauncher, project, runID, payload)
	if result == nil {
		status := w.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return nil, fmt.Errorf("synthetic completion failed with HTTP %d: %s", status, w.body.String())
	}
	return result, nil
}

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *captureResponseWriter) Header() http.Header {
	return w.header
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func (w *captureResponseWriter) WriteHeader(status int) {
	w.status = status
}

func dispatchTimedOutPhase(run RunReport, timeout time.Duration, now time.Time) (string, bool) {
	for _, phase := range run.PhaseExecutions {
		if phase.State != "dispatching" {
			continue
		}
		dispatchedAt, ok := phaseDispatchTime(phase)
		if !ok {
			continue
		}
		if now.Sub(dispatchedAt) >= timeout {
			return phase.Name, true
		}
	}
	if len(run.PhaseExecutions) > 0 || len(run.Attempts) == 0 {
		return "", false
	}
	latest := run.Attempts[len(run.Attempts)-1]
	if latest.CompletedAt != nil {
		return "", false
	}
	if now.Sub(latest.DispatchedAt) >= timeout {
		return firstNonEmpty(latest.Phase, "phase"), true
	}
	return "", false
}

func phaseDispatchTime(phase RunPhaseExecution) (time.Time, bool) {
	if phase.DispatchedAt != nil && *phase.DispatchedAt != "" {
		if ts, err := time.Parse(time.RFC3339Nano, *phase.DispatchedAt); err == nil {
			return ts, true
		}
	}
	if phase.CreatedAt != "" {
		if ts, err := time.Parse(time.RFC3339Nano, phase.CreatedAt); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// ExpireFailedActiveJobs walks in-progress runs and, for each active job
// whose backing k8s Job has terminally failed without a completion callback,
// synthesizes a failed completion through the same path the native runner
// would have used. Without this, a runner pod killed mid-step (DeadlineExceeded,
// OOM, eviction) leaves the run permanently "in_progress" with the phase
// showing as "active" — invisible to dashboards and gates, and impossible for
// the verify-loop budget to retry. See run-execution-reconciler design notes.
func ExpireFailedActiveJobs(ctx context.Context, store RunDispatchTimeoutStore, nativeLauncher NativeLauncher, statusGetter NativeJobStatusGetter, namespace string, urlBuilder LogArchiveURLBuilder, grace time.Duration, now time.Time) (int, error) {
	if statusGetter == nil {
		return 0, nil
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return 0, nil
	}
	completionStore, _ := any(store).(RunCompletionStore)
	jobStore, _ := any(store).(NativeJobCompletionStore)
	if completionStore == nil || jobStore == nil {
		return 0, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, project := range projects {
		name := firstNonEmpty(project.Name, project.ID)
		if name == "" {
			continue
		}
		runs, err := store.ListProjectRuns(ctx, name, 500)
		if err != nil {
			return completed, err
		}
		for _, run := range runs {
			if run.State != "in_progress" || run.ID == "" {
				continue
			}
			n, err := reconcileFailedActiveJobsForRun(ctx, completionStore, jobStore, nativeLauncher, statusGetter, run, namespace, urlBuilder, grace, now)
			if err != nil {
				return completed, err
			}
			completed += n
		}
	}
	return completed, nil
}

func reconcileFailedActiveJobsForRun(ctx context.Context, completionStore RunCompletionStore, jobStore NativeJobCompletionStore, nativeLauncher NativeLauncher, statusGetter NativeJobStatusGetter, run RunReport, namespace string, urlBuilder LogArchiveURLBuilder, grace time.Duration, now time.Time) (int, error) {
	completed := 0
	for _, phase := range run.PhaseExecutions {
		// Only "active" phases can have jobs stuck without callbacks.
		// "dispatching" is handled by ExpireRunDispatchTimeouts.
		if phase.State != "active" {
			continue
		}
		for _, job := range phase.Jobs {
			if job.State != "active" {
				continue
			}
			if job.K8sJobName == nil || strings.TrimSpace(*job.K8sJobName) == "" {
				continue
			}
			ready, conclusion, terminalReason, summary, err := evaluateActiveJobFailure(ctx, statusGetter, namespace, *job.K8sJobName, grace, now)
			if err != nil {
				return completed, err
			}
			if !ready {
				continue
			}
			jobID := job.ID
			// Anchor the log-window at the run's start so the link
			// covers any dispatch + warm-up time too. The completion
			// time is "now" because the runner never delivered one;
			// the synthesized completion writes that timestamp on the
			// attempt anyway.
			logURL := ""
			if urlBuilder != nil {
				logURL = urlBuilder.BuildLogArchiveURL(namespace, *job.K8sJobName, run.StartedAt, now)
			}
			payload := CompletionPayload{
				JobID:           &jobID,
				Conclusion:      conclusion,
				SummaryMarkdown: &summary,
				TerminalReason:  terminalReason,
				LogArchiveURL:   logURL,
			}
			result, err := jobStore.RecordNativeJobCompletion(ctx, run.Project, run.ID, payload)
			if err != nil {
				return completed, err
			}
			completed++
			metrics.RecordRunPhaseJobTerminal(conclusion, NormalizeJobTerminalReason(terminalReason))
			metrics.RecordRunReconcilerCaught("outer")
			if result.CompletionReady {
				if _, err := processSyntheticRunCompletion(ctx, completionStore, nativeLauncher, run.Project, run.ID, result.PhasePayload); err != nil {
					return completed, err
				}
			}
		}
	}
	return completed, nil
}

// evaluateActiveJobFailure asks k8s for the Job's status and, if it is past
// the grace period and terminally failed (or has been garbage-collected from
// k8s entirely), returns the synthetic completion conclusion, the
// JobTerminalReason* enum value, and a summary the dashboard can render.
func evaluateActiveJobFailure(ctx context.Context, statusGetter NativeJobStatusGetter, namespace, name string, grace time.Duration, now time.Time) (bool, string, string, string, error) {
	status, err := statusGetter.GetNativeJobStatus(ctx, namespace, name)
	if err != nil {
		return false, "", "", "", err
	}
	if !status.Found {
		// k8s no longer has the Job (TTL-collected). The run lost its
		// execution surface and cannot be observed further — fail it so
		// the verify loop or cleanup phase can run.
		summary := fmt.Sprintf("native job %q was garbage-collected from kubernetes without a completion callback", name)
		return true, "failed", JobTerminalReasonPodGone, summary, nil
	}
	if status.IsTerminallySucceeded() && !status.IsTerminallyFailed() {
		// Pod ran to completion but the callback was lost. Surface as
		// failed so the run leaves "in_progress"; cleanup phases still
		// run and a human can decide. We prefer this over silently
		// marking success because evidence/verification fields would
		// otherwise be missing.
		if grace > 0 && !status.TerminalTime().IsZero() && now.Sub(status.TerminalTime()) < grace {
			return false, "", "", "", nil
		}
		summary := fmt.Sprintf("native job %q completed in kubernetes but its completion callback was never received", name)
		return true, "failed", JobTerminalReasonCallbackLost, summary, nil
	}
	if !status.IsTerminallyFailed() {
		return false, "", "", "", nil
	}
	if grace > 0 && !status.TerminalTime().IsZero() && now.Sub(status.TerminalTime()) < grace {
		return false, "", "", "", nil
	}
	reason := status.FailureReason()
	message := status.FailureMessage()
	conclusion := "failed"
	terminalReason := JobTerminalReasonJobFailed
	switch reason {
	case "DeadlineExceeded":
		// kubelet killed the pod for hitting activeDeadlineSeconds.
		// Tag as timed_out so dashboards distinguish wallclock kills
		// from a runner-reported failure.
		conclusion = "timed_out"
		terminalReason = JobTerminalReasonDeadlineExceeded
	case "BackoffLimitExceeded":
		// With backoffLimit=0 this usually means the pod itself hit a
		// terminal failure (DeadlineExceeded at the pod level, OOM,
		// crash). The Job controller surfaces it as
		// BackoffLimitExceeded. Treated as timed_out for the same
		// reason as DeadlineExceeded — runner had no chance to report.
		conclusion = "timed_out"
		terminalReason = JobTerminalReasonBackoffExceeded
	}
	summary := fmt.Sprintf("native job %q ended with kubernetes condition Failed=true reason=%q: %s", name, reason, strings.TrimSpace(message))
	return true, conclusion, terminalReason, summary, nil
}

// ExpireInnerJobTerminations walks every in-progress run's
// inner_jobs[] and, for each child still active whose k8s Job has
// terminally succeeded or failed past the grace period, emits an
// inner_job_terminated event. The event flows through the same
// applyNativeEventToExecutionsRaw path runner-emitted events use; the
// run-report API then surfaces the terminal state and reason on the
// inner-Job row.
//
// This is the watcher half of inner-Job Stage 2. The registration
// half (inner_job_registered) is runner-emitted from the marker parser
// (glimmung#625); this terminator half is reconciler-emitted because
// only glimmung has the cluster-wide kube read access to observe the
// child Job's status conditions across slot namespaces.
//
// Idempotency: a seq deterministically derived from the inner Job's
// identity means the same termination emitted twice collides on the
// docID and the second write is silently dropped. The next reconciler
// tick sees the inner Job in state=succeeded/failed and skips it.
func ExpireInnerJobTerminations(ctx context.Context, store RunDispatchTimeoutStore, statusGetter NativeJobStatusGetter, logsFetcher NativeJobLogsFetcher, artifactWriter ArtifactWriter, urlBuilder LogArchiveURLBuilder, grace time.Duration, now time.Time, logf func(string, ...any)) (int, error) {
	if statusGetter == nil {
		return 0, nil
	}
	eventStore, ok := any(store).(NativeRunStore)
	if !ok || eventStore == nil {
		return 0, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	emitted := 0
	for _, project := range projects {
		name := firstNonEmpty(project.Name, project.ID)
		if name == "" {
			continue
		}
		runs, err := store.ListProjectRuns(ctx, name, 500)
		if err != nil {
			return emitted, err
		}
		for _, run := range runs {
			if run.State != "in_progress" || run.ID == "" {
				continue
			}
			for _, phase := range run.PhaseExecutions {
				for _, ij := range phase.InnerJobs {
					if ij.State != "" && ij.State != "active" {
						continue
					}
					if strings.TrimSpace(ij.Namespace) == "" || strings.TrimSpace(ij.JobName) == "" {
						continue
					}
					event, ok, err := buildInnerJobTermination(ctx, statusGetter, logsFetcher, artifactWriter, run.Project, run.ID, phase.Name, ij, urlBuilder, grace, now, logf)
					if err != nil {
						// RBAC / transient errors: skip this child
						// on this tick; the next tick retries.
						continue
					}
					if !ok {
						continue
					}
					if _, err := eventStore.RecordNativeEventByID(ctx, run.Project, run.ID, event); err != nil {
						if errors.Is(err, ErrConflict) {
							// Already emitted — idempotent success.
							continue
						}
						return emitted, err
					}
					emitted++
					metrics.RecordRunReconcilerCaught("inner")
				}
			}
		}
	}
	return emitted, nil
}

// buildInnerJobTermination polls the inner Job's k8s status and, if
// terminal past the grace period, constructs the inner_job_terminated
// event the store applies via applyNativeEventToExecutionsRaw.
func buildInnerJobTermination(ctx context.Context, statusGetter NativeJobStatusGetter, logsFetcher NativeJobLogsFetcher, artifactWriter ArtifactWriter, project, runID, phaseName string, ij InnerJobRef, urlBuilder LogArchiveURLBuilder, grace time.Duration, now time.Time, logf func(string, ...any)) (NativeRunEventRequest, bool, error) {
	status, err := statusGetter.GetNativeJobStatus(ctx, ij.Namespace, ij.JobName)
	if err != nil {
		return NativeRunEventRequest{}, false, err
	}
	var state, reason, completedAt string
	switch {
	case !status.Found:
		// Inner Job TTL'd or externally deleted. No terminal time to
		// grace-defer on; treat as failed/pod_gone immediately.
		state = "failed"
		reason = JobTerminalReasonPodGone
		completedAt = now.UTC().Format(time.RFC3339Nano)
	case status.IsTerminallySucceeded() && !status.IsTerminallyFailed():
		if grace > 0 && !status.TerminalTime().IsZero() && now.Sub(status.TerminalTime()) < grace {
			return NativeRunEventRequest{}, false, nil
		}
		state = "succeeded"
		reason = ""
		completedAt = formatTerminalTime(status.TerminalTime(), now)
	case status.IsTerminallyFailed():
		if grace > 0 && !status.TerminalTime().IsZero() && now.Sub(status.TerminalTime()) < grace {
			return NativeRunEventRequest{}, false, nil
		}
		state = "failed"
		reason = mapK8sFailedReasonToInnerJobReason(status.FailureReason())
		completedAt = formatTerminalTime(status.TerminalTime(), now)
	default:
		return NativeRunEventRequest{}, false, nil
	}
	parentJobID := ij.ParentJobID
	var parentStepSlug *string
	if ij.ParentStepSlug != nil && *ij.ParentStepSlug != "" {
		slug := *ij.ParentStepSlug
		parentStepSlug = &slug
	}
	metadata := map[string]any{
		"namespace":    ij.Namespace,
		"job_name":     ij.JobName,
		"state":        state,
		"reason":       reason,
		"completed_at": completedAt,
		"phase":        phaseName,
	}
	// Prefer the durable artifact-store capture (stage 3 of the
	// inner-Job contract); fall back to the Grafana Loki deep-link
	// when either the launcher cannot fetch logs (no NativeJobLogsFetcher
	// support) or the artifact writer is unconfigured, or the upload
	// failed transiently. The Grafana fallback works while Loki has the
	// data; the artifact URL works forever.
	if logURL := captureInnerJobLogs(ctx, logsFetcher, artifactWriter, project, runID, ij.Namespace, ij.JobName, logf); logURL != "" {
		metadata["log_archive_url"] = logURL
	} else if urlBuilder != nil {
		from := parseInnerJobRegisteredAt(ij.RegisteredAt)
		toTime := now
		if state == "succeeded" || state == "failed" {
			if t, err := time.Parse(time.RFC3339Nano, completedAt); err == nil {
				toTime = t
			}
		}
		if logURL := urlBuilder.BuildLogArchiveURL(ij.Namespace, ij.JobName, from, toTime); logURL != "" {
			metadata["log_archive_url"] = logURL
		}
	}
	return NativeRunEventRequest{
		JobID:    parentJobID,
		Seq:      innerJobTerminationSeq(ij),
		Event:    "inner_job_terminated",
		StepSlug: parentStepSlug,
		Metadata: metadata,
	}, true, nil
}

func parseInnerJobRegisteredAt(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		return t
	}
	return time.Time{}
}

// innerJobTerminationSeq derives a deterministic seq from the inner
// Job's identity so the docID (runID::attemptIndex::jobID::seq)
// dedupes re-emissions across reconciler ticks and process restarts.
// The hash collapses (namespace, job_name) which uniquely identifies
// the inner Job in the cluster. Reconciler seqs start at 2^30 to leave
// runner seqs (which start at 1) plenty of room with no risk of
// collision.
func innerJobTerminationSeq(ij InnerJobRef) int {
	h := fnvHash64(ij.Namespace + "/" + ij.JobName + "::termination")
	const (
		cap31           = 1<<31 - 1
		reconcilerBase  = 1 << 30
		reconcilerRange = cap31 - reconcilerBase
	)
	return reconcilerBase + int(h%uint64(reconcilerRange))
}

// fnvHash64 is FNV-1a 64-bit. Inlined here to avoid adding a hash/fnv
// import just for one helper.
func fnvHash64(s string) uint64 {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// formatTerminalTime returns the RFC3339Nano string for the Job's
// terminal time. Falls back to "now" when the k8s status didn't carry
// a completion timestamp (rare, mostly TTL-collected paths).
func formatTerminalTime(terminal, now time.Time) string {
	if terminal.IsZero() {
		return now.UTC().Format(time.RFC3339Nano)
	}
	return terminal.UTC().Format(time.RFC3339Nano)
}

// mapK8sFailedReasonToInnerJobReason mirrors the outer reconciler's
// k8s reason -> JobTerminalReason* mapping for inner Jobs.
func mapK8sFailedReasonToInnerJobReason(k8sReason string) string {
	switch k8sReason {
	case "DeadlineExceeded":
		return JobTerminalReasonDeadlineExceeded
	case "BackoffLimitExceeded":
		return JobTerminalReasonBackoffExceeded
	default:
		return JobTerminalReasonJobFailed
	}
}
