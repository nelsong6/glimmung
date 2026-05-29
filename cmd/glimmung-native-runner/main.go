package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/agentcost"
)

const (
	defaultWorkspace        = "/workspace"
	defaultAttemptTokenPath = "/var/run/glimmung/attempt-token"
	// shutdownCompleteBudget is the time we reserve from the kubelet's
	// terminationGracePeriodSeconds (default 30s) to deliver the
	// timed_out /completed callback when we receive SIGTERM. The
	// remainder of the grace period covers in-flight HTTP writes and
	// child-process teardown.
	shutdownCompleteBudget = 20 * time.Second
)

type jobSpec struct {
	ID               string            `json:"id"`
	Env              map[string]string `json:"env"`
	Steps            []stepSpec        `json:"steps"`
	Checkout         *checkoutSpec     `json:"checkout"`
	ExtraCheckouts   []checkoutSpec    `json:"extra_checkouts"`
	WorkingDirectory string            `json:"working_directory"`
	Shell            string            `json:"shell"`
}

type stepSpec struct {
	Slug             string            `json:"slug"`
	Type             string            `json:"type"`
	Run              string            `json:"run"`
	Shell            string            `json:"shell"`
	WorkingDirectory string            `json:"working_directory"`
	Env              map[string]string `json:"env"`
}

type checkoutSpec struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

type runnerConfig struct {
	Job            jobSpec
	JobID          string
	AttemptIndex   *int
	EventsURL      string
	CompletedURL   string
	GitHubTokenURL string
	AttemptToken   string
	Workspace      string
}

type nativeRunner struct {
	cfg              runnerConfig
	client           *http.Client
	seq              int
	outputs          map[string]string
	completion       completionMetadata
	githubTokenCache *githubTokenResult
	mu               sync.Mutex
	costUSD          float64
}

type nativeEventRequest struct {
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	AttemptIndex *int           `json:"attempt_index,omitempty"`
	StepSlug     *string        `json:"step_slug,omitempty"`
	Message      *string        `json:"message,omitempty"`
	ExitCode     *int           `json:"exit_code,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type completedRequest struct {
	JobID               string             `json:"job_id"`
	Conclusion          string             `json:"conclusion"`
	AttemptIndex        *int               `json:"attempt_index,omitempty"`
	CostUSD             float64            `json:"cost_usd,omitempty"`
	Verification        map[string]any     `json:"verification,omitempty"`
	Evidence            []evidenceArtifact `json:"evidence,omitempty"`
	ScreenshotsMarkdown *string            `json:"screenshots_markdown,omitempty"`
	SummaryMarkdown     *string            `json:"summary_markdown,omitempty"`
	Outputs             map[string]string  `json:"outputs"`
}

type completionMetadata struct {
	Verification        map[string]any     `json:"verification"`
	Evidence            []evidenceArtifact `json:"evidence"`
	ScreenshotsMarkdown string             `json:"screenshots_markdown"`
	SummaryMarkdown     string             `json:"summary_markdown"`
}

type evidenceArtifact struct {
	Kind         string `json:"kind"`
	Ref          string `json:"ref"`
	Label        string `json:"label"`
	URL          string `json:"url,omitempty"`
	ArtifactPath string `json:"artifact_path,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	DurationMS   int    `json:"duration_ms,omitempty"`
}

type githubTokenResult struct {
	Repo  string `json:"repo"`
	Token string `json:"token"`
}

func main() {
	cfg, err := runnerConfigFromEnv()
	if err != nil {
		log.Printf("configure runner: %v", err)
		os.Exit(1)
	}
	r := &nativeRunner{
		cfg:     cfg,
		client:  &http.Client{Timeout: 30 * time.Second},
		outputs: map[string]string{},
	}
	// Catch SIGTERM (kubelet activeDeadlineSeconds, pod eviction, node
	// drain) and SIGINT (local dev) so we can report a terminal
	// /completed callback before kubelet's SIGKILL lands.
	//
	// Without this, a pod killed mid-step delivers no callback at all
	// and the run sits in_progress forever from glimmung's side.
	// ambience#170/runs/1.1 is the canonical incident; the run-execution
	// reconciler is the safety net for the truly violent paths
	// (OOMKilled, node loss, eviction-without-grace) that fire SIGKILL
	// straight away and skip this handler entirely.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	if err := r.run(signalCtx); err != nil {
		log.Printf("native runner failed: %v", err)
		os.Exit(1)
	}
}

func runnerConfigFromEnv() (runnerConfig, error) {
	rawSpec := strings.TrimSpace(os.Getenv("GLIMMUNG_RUNNER_JOB_SPEC"))
	if rawSpec == "" {
		return runnerConfig{}, errors.New("GLIMMUNG_RUNNER_JOB_SPEC required")
	}
	var job jobSpec
	if err := json.Unmarshal([]byte(rawSpec), &job); err != nil {
		return runnerConfig{}, fmt.Errorf("decode GLIMMUNG_RUNNER_JOB_SPEC: %w", err)
	}
	jobID := firstNonEmpty(os.Getenv("GLIMMUNG_JOB_ID"), job.ID)
	if strings.TrimSpace(jobID) == "" {
		return runnerConfig{}, errors.New("GLIMMUNG_JOB_ID required")
	}
	var attemptIndex *int
	if raw := strings.TrimSpace(os.Getenv("GLIMMUNG_ATTEMPT_INDEX")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return runnerConfig{}, fmt.Errorf("GLIMMUNG_ATTEMPT_INDEX must be an integer: %w", err)
		}
		attemptIndex = &parsed
	}
	token := strings.TrimSpace(os.Getenv("GLIMMUNG_ATTEMPT_TOKEN"))
	if token == "" {
		fromFile, err := os.ReadFile(defaultAttemptTokenPath)
		if err == nil {
			token = strings.TrimSpace(string(fromFile))
		}
	}
	return runnerConfig{
		Job:            job,
		JobID:          jobID,
		AttemptIndex:   attemptIndex,
		EventsURL:      strings.TrimSpace(os.Getenv("GLIMMUNG_EVENTS_URL")),
		CompletedURL:   strings.TrimSpace(os.Getenv("GLIMMUNG_COMPLETED_URL")),
		GitHubTokenURL: strings.TrimSpace(os.Getenv("GLIMMUNG_GITHUB_TOKEN_URL")),
		AttemptToken:   token,
		Workspace:      firstNonEmpty(os.Getenv("GLIMMUNG_WORKSPACE"), defaultWorkspace),
	}, nil
}

func (r *nativeRunner) run(ctx context.Context) error {
	if err := os.MkdirAll(r.cfg.Workspace, 0o755); err != nil {
		_ = r.completeOrShutdown(ctx, "failure", "create workspace: "+err.Error())
		return err
	}
	if err := r.prepareCheckouts(ctx); err != nil {
		if r.shutdownRequested(ctx) {
			return r.completeShutdown(ctx, "runner received shutdown during checkout: "+err.Error())
		}
		_ = r.postEvent(ctx, "runner_failed", nil, "checkout failed: "+err.Error(), nil, nil)
		_ = r.complete(ctx, "failure", "checkout failed: "+err.Error())
		return err
	}
	for _, step := range r.cfg.Job.Steps {
		if strings.TrimSpace(step.Type) == "" {
			step.Type = "run"
		}
		if step.Type != "run" {
			err := fmt.Errorf("step %q uses unsupported type %q", step.Slug, step.Type)
			_ = r.complete(ctx, "failure", err.Error())
			return err
		}
		if err := r.runStep(ctx, step); err != nil {
			if r.shutdownRequested(ctx) {
				slug := strings.TrimSpace(step.Slug)
				return r.completeShutdown(
					ctx,
					fmt.Sprintf("runner received shutdown during step %q: %v", slug, err),
				)
			}
			_ = r.complete(ctx, "failure", err.Error())
			return err
		}
	}
	return r.complete(ctx, "success", "completed")
}

// shutdownRequested reports whether the supplied (signal-aware) context
// has been cancelled. NotifyContext cancels its returned context on the
// first SIGTERM/SIGINT, so this is the precise distinction between
// "child step failed on its own" and "we're being torn down."
func (r *nativeRunner) shutdownRequested(ctx context.Context) bool {
	return ctx.Err() != nil
}

// completeShutdown posts a terminal /completed callback with
// conclusion=timed_out before the kubelet sends SIGKILL. The original
// context has been cancelled by signal.NotifyContext, so we open a
// fresh background context with a tight budget that fits inside the
// pod's terminationGracePeriodSeconds — the in-flight HTTP write is
// the only thing we promise to finish on the way out.
//
// Always returns a non-nil error so r.run propagates the shutdown up
// to main() which exits non-zero. The Job's pod is already terminating;
// the exit code is mostly cosmetic but a clean non-zero is correct
// shape for "the runner was killed before it could finish its work."
func (r *nativeRunner) completeShutdown(_ context.Context, summary string) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownCompleteBudget)
	defer cancel()
	_ = r.postEvent(shutdownCtx, "runner_failed", nil, summary, nil, nil)
	if err := r.complete(shutdownCtx, "timed_out", summary); err != nil {
		log.Printf("shutdown completion callback failed: %v", err)
		return fmt.Errorf("shutdown completion callback failed: %w", err)
	}
	log.Printf("shutdown completion callback delivered: %s", summary)
	return errors.New(summary)
}

// completeOrShutdown picks the right conclusion based on whether the
// supplied context has already been cancelled. Used for early-init
// failures where we don't yet know if SIGTERM raced the failure.
func (r *nativeRunner) completeOrShutdown(ctx context.Context, conclusion, summary string) error {
	if r.shutdownRequested(ctx) {
		return r.completeShutdown(ctx, summary)
	}
	return r.complete(ctx, conclusion, summary)
}

func (r *nativeRunner) runStep(ctx context.Context, step stepSpec) error {
	slug := strings.TrimSpace(step.Slug)
	if slug == "" {
		return errors.New("step slug required")
	}
	if err := r.postEvent(ctx, "step_started", &slug, "", nil, nil); err != nil {
		return err
	}
	outputFile := filepath.Join(os.TempDir(), "glimmung-output-"+slug+".txt")
	completionFile := filepath.Join(os.TempDir(), "glimmung-completion-"+slug+".json")
	_ = os.Remove(outputFile)
	_ = os.Remove(completionFile)
	exitCode, execErr := r.executeStep(ctx, step, outputFile, completionFile)
	outputs, outputErr := parseOutputFile(outputFile)
	if outputErr == nil {
		outputErr = r.publishOutputs(ctx, slug, outputs)
	}
	if completionErr := r.collectCompletionMetadata(completionFile); outputErr == nil && completionErr != nil {
		outputErr = completionErr
	}
	if execErr != nil {
		msg := fmt.Sprintf("step %s exited with code %d", slug, exitCode)
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exitCode, nil)
		return fmt.Errorf("%s: %w", msg, execErr)
	}
	if outputErr != nil {
		exit := 1
		msg := "step " + slug + " output error: " + outputErr.Error()
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exit, nil)
		return errors.New(msg)
	}
	if err := r.postEvent(ctx, "step_completed", &slug, "", &exitCode, nil); err != nil {
		return err
	}
	return nil
}

func (r *nativeRunner) executeStep(ctx context.Context, step stepSpec, outputFile, completionFile string) (int, error) {
	workdir := firstNonEmpty(step.WorkingDirectory, r.cfg.Job.WorkingDirectory, r.cfg.Workspace)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return 1, err
	}
	name, args := shellCommand(firstNonEmpty(step.Shell, r.cfg.Job.Shell), step.Run)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workdir
	cmd.Env = mergedEnv(os.Environ(), r.cfg.Job.Env, step.Env, map[string]string{
		"GLIMMUNG_MANAGED_RUNNER":  "1",
		"GLIMMUNG_OUTPUT_FILE":     outputFile,
		"GLIMMUNG_COMPLETION_FILE": completionFile,
		"GLIMMUNG_STEP_SLUG":       step.Slug,
	})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go r.streamLogs(ctx, &wg, step.Slug, "stdout", stdout)
	go r.streamLogs(ctx, &wg, step.Slug, "stderr", stderr)
	wg.Wait()
	waitErr := cmd.Wait()
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), waitErr
	}
	return 1, waitErr
}

func shellCommand(shell, script string) (string, []string) {
	switch strings.TrimSpace(shell) {
	case "", "bash":
		return "bash", []string{"-e", "-u", "-o", "pipefail", "-c", script}
	case "sh":
		return "sh", []string{"-e", "-u", "-c", script}
	default:
		fields := strings.Fields(shell)
		if len(fields) == 0 {
			return "bash", []string{"-e", "-u", "-o", "pipefail", "-c", script}
		}
		return fields[0], append(fields[1:], "-c", script)
	}
}

func (r *nativeRunner) streamLogs(ctx context.Context, wg *sync.WaitGroup, stepSlug, stream string, reader io.Reader) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		r.observeLogCost(line)
		if stream == "stderr" {
			fmt.Fprintln(os.Stderr, line)
		} else {
			fmt.Println(line)
		}
		_ = r.postEvent(ctx, "log", &stepSlug, line, nil, map[string]any{"stream": stream})
	}
}

func (r *nativeRunner) publishOutputs(ctx context.Context, stepSlug string, outputs map[string]string) error {
	keys := make([]string, 0, len(outputs))
	for key := range outputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := r.outputs[key]; exists {
			return fmt.Errorf("phase output %q already set", key)
		}
		value := outputs[key]
		if err := r.postEvent(ctx, "phase_output_set", &stepSlug, "", nil, map[string]any{
			"key":         key,
			"value":       value,
			"source_step": stepSlug,
		}); err != nil {
			return err
		}
		r.outputs[key] = value
	}
	return nil
}

func (r *nativeRunner) prepareCheckouts(ctx context.Context) error {
	if r.cfg.Job.Checkout != nil {
		if err := r.checkout(ctx, *r.cfg.Job.Checkout); err != nil {
			return err
		}
	}
	for _, checkout := range r.cfg.Job.ExtraCheckouts {
		if err := r.checkout(ctx, checkout); err != nil {
			return err
		}
	}
	return nil
}

func (r *nativeRunner) checkout(ctx context.Context, checkout checkoutSpec) error {
	token, err := r.githubToken(ctx)
	if err != nil {
		return err
	}
	repo := firstNonEmpty(checkout.Repo, token.Repo)
	if repo == "" {
		return errors.New("checkout repo required")
	}
	path := checkout.Path
	if path == "" {
		path = filepath.Join(r.cfg.Workspace, repoBaseName(repo))
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("checkout path already exists: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	url := "https://x-access-token:" + token.Token + "@github.com/" + repo + ".git"
	if err := runCapture(ctx, "", "git", "clone", url, path); err != nil {
		return scrubToken(err, token.Token)
	}
	if ref := strings.TrimSpace(checkout.Ref); ref != "" {
		if err := runCapture(ctx, path, "git", "checkout", ref); err != nil {
			return scrubToken(err, token.Token)
		}
	}
	return nil
}

func (r *nativeRunner) githubToken(ctx context.Context) (githubTokenResult, error) {
	if r.githubTokenCache != nil {
		return *r.githubTokenCache, nil
	}
	if r.cfg.GitHubTokenURL == "" {
		return githubTokenResult{}, errors.New("GLIMMUNG_GITHUB_TOKEN_URL required for checkout")
	}
	var result githubTokenResult
	if err := r.postJSON(ctx, r.cfg.GitHubTokenURL, map[string]any{}, &result); err != nil {
		return githubTokenResult{}, err
	}
	if result.Token == "" {
		return githubTokenResult{}, errors.New("GitHub token response did not include token")
	}
	r.githubTokenCache = &result
	return result, nil
}

func runCapture(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *nativeRunner) postEvent(ctx context.Context, event string, stepSlug *string, message string, exitCode *int, metadata map[string]any) error {
	if r.cfg.EventsURL == "" {
		return nil
	}
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	var messagePtr *string
	if message != "" {
		messagePtr = &message
	}
	req := nativeEventRequest{
		JobID:        r.cfg.JobID,
		Seq:          seq,
		Event:        event,
		AttemptIndex: r.cfg.AttemptIndex,
		StepSlug:     stepSlug,
		Message:      messagePtr,
		ExitCode:     exitCode,
		Metadata:     metadata,
	}
	return r.postJSON(ctx, r.cfg.EventsURL, req, nil)
}

func (r *nativeRunner) complete(ctx context.Context, conclusion, summary string) error {
	if r.cfg.CompletedURL == "" {
		return nil
	}
	req := completedRequest{
		JobID:        r.cfg.JobID,
		Conclusion:   conclusion,
		AttemptIndex: r.cfg.AttemptIndex,
		CostUSD:      r.observedCostUSD(),
		Outputs:      r.outputs,
	}
	if len(r.completion.Verification) > 0 {
		req.Verification = r.completion.Verification
	}
	if len(r.completion.Evidence) > 0 {
		req.Evidence = r.completion.Evidence
	}
	if strings.TrimSpace(r.completion.ScreenshotsMarkdown) != "" {
		req.ScreenshotsMarkdown = &r.completion.ScreenshotsMarkdown
	}
	if strings.TrimSpace(r.completion.SummaryMarkdown) != "" {
		req.SummaryMarkdown = &r.completion.SummaryMarkdown
	} else if strings.TrimSpace(summary) != "" {
		req.SummaryMarkdown = &summary
	}
	return r.postJSON(ctx, r.cfg.CompletedURL, req, nil)
}

func (r *nativeRunner) observeLogCost(line string) {
	cost, ok := agentcost.FromJSONLogLine(line)
	if !ok {
		return
	}
	r.mu.Lock()
	r.costUSD += cost
	r.mu.Unlock()
}

func (r *nativeRunner) observedCostUSD() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.costUSD
}

func (r *nativeRunner) collectCompletionMetadata(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var metadata completionMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	if len(metadata.Verification) > 0 {
		r.completion.Verification = metadata.Verification
	}
	if len(metadata.Evidence) > 0 {
		r.completion.Evidence = metadata.Evidence
	}
	if strings.TrimSpace(metadata.ScreenshotsMarkdown) != "" {
		r.completion.ScreenshotsMarkdown = metadata.ScreenshotsMarkdown
	}
	if strings.TrimSpace(metadata.SummaryMarkdown) != "" {
		r.completion.SummaryMarkdown = metadata.SummaryMarkdown
	}
	return nil
}

func (r *nativeRunner) postJSON(ctx context.Context, url string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.AttemptToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.AttemptToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return err
		}
	}
	return nil
}

func parseOutputFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	outputs := map[string]string{}
	if bytes.HasPrefix(raw, []byte("{")) {
		if err := mergeOutputJSON(outputs, raw); err == nil {
			return outputs, nil
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") {
			if err := mergeOutputJSON(outputs, []byte(line)); err != nil {
				return nil, err
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid output line %q", line)
		}
		if err := setOutput(outputs, strings.TrimSpace(key), value); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return outputs, nil
}

func mergeOutputJSON(outputs map[string]string, raw []byte) error {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	if keyRaw, ok := obj["key"]; ok {
		key := strings.TrimSpace(fmt.Sprint(keyRaw))
		value := stringifyOutputValue(obj["value"])
		return setOutput(outputs, key, value)
	}
	for key, value := range obj {
		if err := setOutput(outputs, strings.TrimSpace(key), stringifyOutputValue(value)); err != nil {
			return err
		}
	}
	return nil
}

func setOutput(outputs map[string]string, key, value string) error {
	if key == "" {
		return errors.New("output key required")
	}
	if _, exists := outputs[key]; exists {
		return fmt.Errorf("output %q declared more than once", key)
	}
	outputs[key] = value
	return nil
}

func stringifyOutputValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64, bool:
		return fmt.Sprint(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(raw)
	}
}

func mergedEnv(base []string, maps ...map[string]string) []string {
	values := map[string]string{}
	order := make([]string, 0, len(base))
	for _, row := range base {
		key, value, ok := strings.Cut(row, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for _, m := range maps {
		for key, value := range m {
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = value
		}
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+values[key])
	}
	return out
}

func repoBaseName(repo string) string {
	repo = strings.TrimSuffix(repo, ".git")
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
