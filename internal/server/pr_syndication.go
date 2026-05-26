package server

import (
	"context"
	"encoding/json"
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

type prPrimitiveStore interface {
	ReadIssueForDispatch(ctx context.Context, project string, issueNumber int) (IssueDispatchData, error)
	LinkRunPullRequest(ctx context.Context, project, runID string, prNumber int) error
	EnsureTouchpoint(ctx context.Context, req TouchpointCreate) (TouchpointDetail, error)
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

func nativePRTouchpointByCallbackToken(store ReadStore, prClient PullRequestClient) http.HandlerFunc {
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
		if wf == nil || !wf.PR.Enabled {
			writeJSON(w, http.StatusOK, PRPrimitiveResult{Status: "skipped", Reason: "workflow PR primitive is disabled"})
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
		result, err := materializePRPrimitive(r.Context(), completionStore, prClient, run)
		if err != nil {
			writeInternalError(w, r, err, "ensure PR touchpoint failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
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
	if latestPrimary.Decision == "" && latestPrimary.Completed && latestPrimary.Conclusion == "success" {
		return true, ""
	}
	return false, "latest primary phase has not advanced"
}

func materializePRPrimitive(ctx context.Context, store RunCompletionStore, prClient PullRequestClient, run RunReplayData) (PRPrimitiveResult, error) {
	if prClient == nil {
		return PRPrimitiveResult{}, errors.New("pull request client not configured")
	}
	prStore, ok := any(store).(prPrimitiveStore)
	if !ok || prStore == nil {
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
	issue, err := prStore.ReadIssueForDispatch(ctx, run.Project, run.IssueNumber)
	if err != nil {
		return PRPrimitiveResult{}, fmt.Errorf("read issue: %w", err)
	}
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = fmt.Sprintf("Address %s", publicids.IssueRef(run.Project, &run.IssueNumber))
	}
	body := prBodyForRun(run, issue, branch)
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
	if err := prStore.LinkRunPullRequest(ctx, run.Project, run.ID, pr.Number); err != nil {
		return PRPrimitiveResult{}, fmt.Errorf("link run to PR: %w", err)
	}
	runRef := runRefFromData(run)
	issueRef := publicids.IssueRef(run.Project, &run.IssueNumber)
	touchpoint, err := prStore.EnsureTouchpoint(ctx, TouchpointCreate{
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
	if value := phaseOutput(run, "branch_name"); value != "" {
		return value
	}
	if run.RunDisplayNumber != nil && strings.TrimSpace(*run.RunDisplayNumber) != "" && run.IssueNumber > 0 {
		return fmt.Sprintf("issue-%d-run-%s", run.IssueNumber, strings.TrimSpace(*run.RunDisplayNumber))
	}
	return ""
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

func prBodyForRun(run RunReplayData, issue IssueDispatchData, branch string) string {
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
	fmt.Fprintf(&b, "- Branch: `%s`\n", branch)
	if validationURL := runValidationURL(run); validationURL != "" {
		fmt.Fprintf(&b, "- Validation: %s\n", validationURL)
	}
	if summary := phaseOutputJSONField(run, "implementation", "summary"); summary != "" {
		fmt.Fprintf(&b, "\n## Implementation Summary\n\n%s\n", summary)
	}
	if verificationStatus := phaseOutputJSONField(run, "verification", "status"); verificationStatus != "" {
		fmt.Fprintf(&b, "\n## Verification\n\nStatus: `%s`\n", verificationStatus)
	}
	if run.ScreenshotsMarkdown != nil && strings.TrimSpace(*run.ScreenshotsMarkdown) != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(*run.ScreenshotsMarkdown))
	}
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

func runValidationURL(run RunReplayData) string {
	if run.ValidationURL != nil && strings.TrimSpace(*run.ValidationURL) != "" {
		return strings.TrimSpace(*run.ValidationURL)
	}
	return phaseOutput(run, "validation_url")
}

func phaseOutputJSONField(run RunReplayData, outputKey, field string) string {
	raw := phaseOutput(run, outputKey)
	if raw == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	value, _ := payload[field].(string)
	return strings.TrimSpace(value)
}
