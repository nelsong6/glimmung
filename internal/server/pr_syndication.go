package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/nelsong6/glimmung/internal/domain/decision"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

type PullRequestClient interface {
	EnsurePullRequest(ctx context.Context, req PullRequestEnsureRequest) (PullRequest, error)
	MergePullRequest(ctx context.Context, req PullRequestMergeRequest) (PullRequestMergeResult, error)
}

type PullRequestEnsureRequest struct {
	Repo  string
	Base  string
	Head  string
	Title string
	Body  string
}

type PullRequest struct {
	Number  int
	Title   string
	Body    string
	Branch  string
	BaseRef string
	HeadSHA string
	HTMLURL string
	State   string
}

// PullRequestMergeRequest carries the parameters for an idempotent merge.
type PullRequestMergeRequest struct {
	Repo        string
	Number      int
	CommitTitle string
	MergeMethod string
}

// PullRequestMergeResult records the outcome of MergePullRequest. When
// AlreadyMerged is true the PR was merged on a prior call and the request
// is a no-op success.
type PullRequestMergeResult struct {
	Number         int
	HTMLURL        string
	State          string
	MergeCommitSHA string
	AlreadyMerged  bool
}

type prPrimitiveStore interface {
	ReadIssueForDispatch(ctx context.Context, project string, issueNumber int) (IssueDispatchData, error)
	NormalizeRunReviewFacts(ctx context.Context, project, runID string, facts RunReviewFacts) (RunReplayData, error)
	LinkRunPullRequest(ctx context.Context, project, runID string, prNumber int) error
	EnsureTouchpoint(ctx context.Context, req TouchpointCreate) (TouchpointDetail, error)
}

type runTouchpointFinalizeStore interface {
	ReadRunIDForNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, string, error)
	ReadRunForReplay(ctx context.Context, project, runID string) (RunReplayData, error)
	workflowReadStore
	prPrimitiveStore
}

type PRPrimitiveResult struct {
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	Repo           string `json:"repo,omitempty"`
	PRNumber       int    `json:"pr_number,omitempty"`
	Title          string `json:"title,omitempty"`
	Branch         string `json:"branch,omitempty"`
	BaseRef        string `json:"base_ref,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	HTMLURL        string `json:"html_url,omitempty"`
	TouchpointRef  string `json:"touchpoint_ref,omitempty"`
	LinkedIssueRef string `json:"linked_issue_ref,omitempty"`
	LinkedRunRef   string `json:"linked_run_ref,omitempty"`
}

// PRMergeResult is the response body for the pr-merge endpoint.
//
// Status is one of:
//   - "merged"          — Glimmung performed the merge in this call.
//   - "already_merged"  — the PR was merged on a prior call; idempotent
//                         no-op success.
//
// Non-success outcomes are surfaced as a problem response (4xx/5xx) rather
// than this body.
type PRMergeResult struct {
	Status         string `json:"status"`
	Repo           string `json:"repo,omitempty"`
	PRNumber       int    `json:"pr_number,omitempty"`
	HTMLURL        string `json:"html_url,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
}

type RunReviewFacts struct {
	ValidationURL *string
}

func nativePRTouchpointByCallbackToken(store ReadStore, prClient PullRequestClient, artifactStore ArtifactStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "completion store not configured")
			return
		}
		runID, project, _, err := completionStore.ReadRunIDForCallbackToken(r.Context(), r.PathValue("callback_token"))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run by callback token failed")
			return
		}
		run, err := completionStore.ReadRunForReplay(r.Context(), project, runID)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run failed")
			return
		}
		wf, err := workflowForRun(r.Context(), completionStore, run)
		if err != nil {
			writeInternalError(w, r, err, "read workflow failed")
			return
		}
		if wf == nil {
			writeJSON(w, http.StatusOK, PRPrimitiveResult{Status: "skipped", Reason: "workflow not found for run"})
			return
		}
		if ready, reason := prPrimitiveReadyForRun(wf, run); !ready {
			writeJSON(w, http.StatusOK, PRPrimitiveResult{Status: "skipped", Reason: reason})
			return
		}
		if prClient == nil {
			writeProblem(w, http.StatusServiceUnavailable, "pull request client not configured")
			return
		}
		prStore, ok := any(completionStore).(prPrimitiveStore)
		if !ok || prStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "PR primitive store not configured")
			return
		}
		run, err = normalizeRunReviewFacts(r.Context(), prStore, run)
		if err != nil {
			writeInternalError(w, r, err, "normalize run review facts failed")
			return
		}
		result, err := materializePRPrimitive(r.Context(), prStore, prClient, artifactStore, run)
		if err != nil {
			var validationErr ValidationError
			if errors.As(err, &validationErr) {
				writeProblem(w, http.StatusUnprocessableEntity, validationErr.Message)
				return
			}
			writeInternalError(w, r, err, "ensure PR touchpoint failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// nativePRMergeByCallbackToken handles
// POST /v1/run-callbacks/{callback_token}/native/pr-merge — the endpoint
// the managed pr_merge primitive curl's into during its run step. The
// endpoint is idempotent: a second call after the PR is already merged
// returns status "already_merged" without contacting GitHub for a write.
func nativePRMergeByCallbackToken(store ReadStore, prClient PullRequestClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "completion store not configured")
			return
		}
		runID, project, _, err := completionStore.ReadRunIDForCallbackToken(r.Context(), r.PathValue("callback_token"))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run by callback token failed")
			return
		}
		run, err := completionStore.ReadRunForReplay(r.Context(), project, runID)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run failed")
			return
		}
		result, err := mergeRunPullRequest(r.Context(), prClient, run)
		if err != nil {
			var validationErr ValidationError
			if errors.As(err, &validationErr) {
				writeProblem(w, http.StatusUnprocessableEntity, validationErr.Message)
				return
			}
			writeInternalError(w, r, err, "merge pull request failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// mergeRunPullRequest performs the idempotent merge for a run's linked PR.
// The run must have run.PRNumber set; otherwise the caller is asking to
// merge a PR that doesn't exist yet, which is a validation error.
func mergeRunPullRequest(ctx context.Context, prClient PullRequestClient, run RunReplayData) (PRMergeResult, error) {
	if prClient == nil {
		return PRMergeResult{}, fmt.Errorf("pull request client not configured")
	}
	if run.PRNumber == nil || *run.PRNumber < 1 {
		return PRMergeResult{}, ValidationError{Message: "run has no linked PR to merge"}
	}
	repo := strings.TrimSpace(run.IssueRepo)
	if repo == "" {
		return PRMergeResult{}, ValidationError{Message: "run is missing issue_repo; cannot resolve target repo for merge"}
	}
	out, err := prClient.MergePullRequest(ctx, PullRequestMergeRequest{
		Repo:        repo,
		Number:      *run.PRNumber,
		MergeMethod: "merge",
		CommitTitle: fmt.Sprintf("Glimmung touchpoint approve: %s", runRefFromData(run)),
	})
	if err != nil {
		return PRMergeResult{}, err
	}
	status := "merged"
	if out.AlreadyMerged {
		status = "already_merged"
	}
	return PRMergeResult{
		Status:         status,
		Repo:           repo,
		PRNumber:       out.Number,
		HTMLURL:        out.HTMLURL,
		MergeCommitSHA: out.MergeCommitSHA,
	}, nil
}

// mergeRunTouchpointByNumber handles
// POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/touchpoint/merge
// — admin operator endpoint mirroring the touchpoint/finalize shape.
// Idempotent. Useful for triggering an approve action from the API or for
// repairing a stuck gate.
func mergeRunTouchpointByNumber(store ReadStore, prClient PullRequestClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runNumber := strings.TrimSpace(r.PathValue("run_number"))
		if runNumber == "" {
			writeProblem(w, http.StatusBadRequest, "run_number required")
			return
		}
		mergeRunTouchpoint(w, r, store, prClient, runNumber)
	}
}

func mergeRunCycleTouchpointByNumber(store ReadStore, prClient PullRequestClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runNumber := strings.TrimSpace(r.PathValue("run_number"))
		if runNumber == "" {
			writeProblem(w, http.StatusBadRequest, "run_number required")
			return
		}
		if strings.Contains(runNumber, ".") {
			writeProblem(w, http.StatusBadRequest, "run_number must be the base run number when cycle_number is present")
			return
		}
		cycleNumber, ok := positivePathInt(w, r, "cycle_number")
		if !ok {
			return
		}
		mergeRunTouchpoint(w, r, store, prClient, fmt.Sprintf("%s.%d", runNumber, cycleNumber))
	}
}

func mergeRunTouchpoint(w http.ResponseWriter, r *http.Request, store ReadStore, prClient PullRequestClient, runNumber string) {
	finalizeStore, ok := store.(runTouchpointFinalizeStore)
	if !ok || finalizeStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "run touchpoint finalize store not configured")
		return
	}
	issueNumber, ok := positivePathInt(w, r, "issue_number")
	if !ok {
		return
	}
	runID, _, err := finalizeStore.ReadRunIDForNumber(r.Context(), r.PathValue("project"), issueNumber, runNumber)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run by number failed")
		return
	}
	run, err := finalizeStore.ReadRunForReplay(r.Context(), r.PathValue("project"), runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return
	}
	result, err := mergeRunPullRequest(r.Context(), prClient, run)
	if err != nil {
		var validationErr ValidationError
		if errors.As(err, &validationErr) {
			writeProblem(w, http.StatusUnprocessableEntity, validationErr.Message)
			return
		}
		writeInternalError(w, r, err, "merge pull request failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func finalizeRunTouchpointByNumber(store ReadStore, prClient PullRequestClient, artifactStore ArtifactStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runNumber := strings.TrimSpace(r.PathValue("run_number"))
		if runNumber == "" {
			writeProblem(w, http.StatusBadRequest, "run_number required")
			return
		}
		finalizeRunTouchpoint(w, r, store, prClient, artifactStore, runNumber)
	}
}

func finalizeRunCycleTouchpointByNumber(store ReadStore, prClient PullRequestClient, artifactStore ArtifactStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runNumber := strings.TrimSpace(r.PathValue("run_number"))
		if runNumber == "" {
			writeProblem(w, http.StatusBadRequest, "run_number required")
			return
		}
		if strings.Contains(runNumber, ".") {
			writeProblem(w, http.StatusBadRequest, "run_number must be the base run number when cycle_number is present")
			return
		}
		cycleNumber, ok := positivePathInt(w, r, "cycle_number")
		if !ok {
			return
		}
		finalizeRunTouchpoint(w, r, store, prClient, artifactStore, fmt.Sprintf("%s.%d", runNumber, cycleNumber))
	}
}

func finalizeRunTouchpoint(w http.ResponseWriter, r *http.Request, store ReadStore, prClient PullRequestClient, artifactStore ArtifactStore, runNumber string) {
	finalizeStore, ok := store.(runTouchpointFinalizeStore)
	if !ok || finalizeStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "touchpoint finalizer store not configured")
		return
	}
	if prClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "pull request client not configured")
		return
	}
	project := r.PathValue("project")
	issueNumber, ok := positivePathInt(w, r, "issue_number")
	if !ok {
		return
	}
	runID, _, err := finalizeStore.ReadRunIDForNumber(r.Context(), project, issueNumber, runNumber)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return
	}
	run, err := finalizeStore.ReadRunForReplay(r.Context(), project, runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return
	}
	wf, err := workflowForRun(r.Context(), finalizeStore, run)
	if err != nil {
		writeInternalError(w, r, err, "read workflow failed")
		return
	}
	if wf == nil {
		writeProblem(w, http.StatusConflict, "workflow not found for run")
		return
	}
	if ready, reason := prPrimitiveReadyForRun(wf, run); !ready {
		writeProblem(w, http.StatusConflict, reason)
		return
	}
	if strings.TrimSpace(run.IssueRepo) == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "run has no issue_repo")
		return
	}
	normalized, err := normalizeRunReviewFacts(r.Context(), finalizeStore, run)
	if err != nil {
		writeInternalError(w, r, err, "normalize run review facts failed")
		return
	}
	run = normalized
	if prBranchForRun(run) == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "run did not emit a branch_name output; if this is a recycled run, finalize the cycle route /runs/{run_number}/cycles/{cycle_number}/touchpoint/finalize")
		return
	}
	result, err := materializePRPrimitive(r.Context(), finalizeStore, prClient, artifactStore, run)
	if err != nil {
		var validationErr ValidationError
		if errors.As(err, &validationErr) {
			writeProblem(w, http.StatusUnprocessableEntity, validationErr.Message)
			return
		}
		writeInternalError(w, r, err, "finalize touchpoint failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func prPrimitiveReadyForRun(wf *Workflow, run RunReplayData) (bool, string) {
	var latestPrimary *RunAttemptData
	for i := range run.Attempts {
		attempt := &run.Attempts[i]
		phase := phaseSpecByName(wf.Phases, attempt.Phase)
		if phase != nil && phase.Always {
			continue
		}
		if !attempt.Completed && attempt.Decision == "" {
			return false, "primary phase is still in progress"
		}
		if isAbortDecision(attempt.Decision) {
			return false, "run is on an abort path"
		}
		latestPrimary = attempt
	}
	if latestPrimary == nil {
		return false, "run has no primary phase attempts"
	}
	if latestPrimary.Decision == string(decision.Advance) {
		return true, ""
	}
	if latestPrimary.Decision == "" && latestPrimary.Completed && decision.IsAdvanceConclusion(latestPrimary.Conclusion) {
		return true, ""
	}
	return false, "latest primary phase has not advanced"
}

func materializePRPrimitive(ctx context.Context, store prPrimitiveStore, prClient PullRequestClient, artifactStore ArtifactStore, run RunReplayData) (PRPrimitiveResult, error) {
	if prClient == nil {
		return PRPrimitiveResult{}, errors.New("pull request client not configured")
	}
	if store == nil {
		return PRPrimitiveResult{}, errors.New("store does not support PR primitive materialization")
	}
	repo := strings.TrimSpace(run.IssueRepo)
	if repo == "" {
		return PRPrimitiveResult{}, errors.New("run has no issue_repo")
	}
	branch := prBranchForRun(run)
	if branch == "" {
		return PRPrimitiveResult{}, errors.New("run did not emit a branch_name output")
	}
	evidence, err := touchpointEvidenceForRun(ctx, artifactStore, run)
	if err != nil {
		return PRPrimitiveResult{}, err
	}
	issue, err := store.ReadIssueForDispatch(ctx, run.Project, run.IssueNumber)
	if err != nil {
		return PRPrimitiveResult{}, fmt.Errorf("read issue: %w", err)
	}
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = fmt.Sprintf("Address %s", publicids.IssueRef(run.Project, &run.IssueNumber))
	}
	body := prBodyForRun(run, issue)
	pr, err := prClient.EnsurePullRequest(ctx, PullRequestEnsureRequest{
		Repo:  repo,
		Base:  "main",
		Head:  branch,
		Title: title,
		Body:  body,
	})
	if err != nil {
		return PRPrimitiveResult{}, fmt.Errorf("ensure GitHub PR: %w", err)
	}
	if pr.Number < 1 {
		return PRPrimitiveResult{}, errors.New("GitHub PR response did not include a positive number")
	}
	if err := store.LinkRunPullRequest(ctx, run.Project, run.ID, pr.Number); err != nil {
		return PRPrimitiveResult{}, fmt.Errorf("link run to PR: %w", err)
	}
	runRef := runRefFromData(run)
	issueRef := publicids.IssueRef(run.Project, &run.IssueNumber)
	touchpoint, err := store.EnsureTouchpoint(ctx, TouchpointCreate{
		Project:        run.Project,
		Repo:           repo,
		Number:         pr.Number,
		Title:          firstNonEmpty(pr.Title, title),
		Branch:         firstNonEmpty(pr.Branch, branch),
		Body:           firstNonEmpty(pr.Body, body),
		BaseRef:        firstNonEmpty(pr.BaseRef, "main"),
		HeadSHA:        pr.HeadSHA,
		HTMLURL:        pr.HTMLURL,
		LinkedIssueRef: issueRef,
		LinkedRunRef:   runRef,
		Evidence:       evidence,
		EvidenceSet:    true,
	})
	if err != nil {
		return PRPrimitiveResult{}, fmt.Errorf("ensure touchpoint: %w", err)
	}
	return PRPrimitiveResult{
		Status:         "ensured",
		Repo:           repo,
		PRNumber:       pr.Number,
		Title:          firstNonEmpty(pr.Title, title),
		Branch:         firstNonEmpty(pr.Branch, branch),
		BaseRef:        firstNonEmpty(pr.BaseRef, "main"),
		HeadSHA:        pr.HeadSHA,
		HTMLURL:        pr.HTMLURL,
		TouchpointRef:  touchpoint.Ref,
		LinkedIssueRef: issueRef,
		LinkedRunRef:   runRef,
	}, nil
}

func prBranchForRun(run RunReplayData) string {
	return phaseOutput(run, "branch_name")
}

func normalizeRunReviewFacts(ctx context.Context, store prPrimitiveStore, run RunReplayData) (RunReplayData, error) {
	facts := runReviewFactsForRun(run)
	if !facts.hasValues() {
		return run, nil
	}
	return store.NormalizeRunReviewFacts(ctx, run.Project, run.ID, facts)
}

func runReviewFactsForRun(run RunReplayData) RunReviewFacts {
	validationURL := ""
	if run.ValidationURL != nil {
		validationURL = strings.TrimSpace(*run.ValidationURL)
	}
	if validationURL == "" {
		validationURL = phaseOutput(run, "validation_url")
	}
	return RunReviewFacts{ValidationURL: stringPtrFromTrimmed(validationURL)}
}

func (f RunReviewFacts) hasValues() bool {
	return f.ValidationURL != nil && strings.TrimSpace(*f.ValidationURL) != ""
}

func stringPtrFromTrimmed(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func phaseOutput(run RunReplayData, key string) string {
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		if run.Attempts[i].PhaseOutputs == nil {
			continue
		}
		value := strings.TrimSpace(run.Attempts[i].PhaseOutputs[key])
		if value != "" {
			return value
		}
	}
	return ""
}

func prBodyForRun(run RunReplayData, issue IssueDispatchData) string {
	issueRef := publicids.IssueRef(run.Project, &run.IssueNumber)
	runRef := runRefFromData(run)
	runURL := fmt.Sprintf("https://glimmung.romaine.life/projects/%s/issues/%d/runs/%s",
		url.PathEscape(run.Project),
		run.IssueNumber,
		url.PathEscape(runDisplayForURL(run)),
	)
	touchpointURL := fmt.Sprintf("https://glimmung.romaine.life/projects/%s/issues/%d/touchpoint",
		url.PathEscape(run.Project),
		run.IssueNumber,
	)
	var b strings.Builder
	fmt.Fprintf(&b, "## Glimmung\n\n")
	fmt.Fprintf(&b, "- Issue: %s\n", issueRef)
	if strings.TrimSpace(issue.Title) != "" {
		fmt.Fprintf(&b, "- Title: %s\n", strings.TrimSpace(issue.Title))
	}
	fmt.Fprintf(&b, "- Run: [%s](%s)\n", runRef, runURL)
	fmt.Fprintf(&b, "- Touchpoint: %s\n", touchpointURL)
	fmt.Fprintf(&b, "\nGlimmung issue: %s\n", issueRef)
	return b.String()
}

func runDisplayForURL(run RunReplayData) string {
	if run.RunDisplayNumber != nil && strings.TrimSpace(*run.RunDisplayNumber) != "" {
		return strings.TrimSpace(*run.RunDisplayNumber)
	}
	if run.RunNumber != nil && *run.RunNumber > 0 {
		return fmt.Sprintf("%d", *run.RunNumber)
	}
	return run.ID
}
