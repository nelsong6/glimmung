package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

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

func materializePRPrimitive(ctx context.Context, store RunCompletionStore, prClient PullRequestClient, run RunReplayData) error {
	if prClient == nil {
		return errors.New("pull request client not configured")
	}
	prStore, ok := any(store).(prPrimitiveStore)
	if !ok || prStore == nil {
		return errors.New("store does not support PR primitive materialization")
	}
	repo := strings.TrimSpace(run.IssueRepo)
	if repo == "" {
		return errors.New("run has no issue_repo")
	}
	branch := prBranchForRun(run)
	if branch == "" {
		return errors.New("run did not emit a branch_name output")
	}
	issue, err := prStore.ReadIssueForDispatch(ctx, run.Project, run.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue: %w", err)
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
		return fmt.Errorf("ensure GitHub PR: %w", err)
	}
	if pr.Number < 1 {
		return errors.New("GitHub PR response did not include a positive number")
	}
	if err := prStore.LinkRunPullRequest(ctx, run.Project, run.ID, pr.Number); err != nil {
		return fmt.Errorf("link run to PR: %w", err)
	}
	runRef := runRefFromData(run)
	issueRef := publicids.IssueRef(run.Project, &run.IssueNumber)
	if _, err := prStore.EnsureTouchpoint(ctx, TouchpointCreate{
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
	}); err != nil {
		return fmt.Errorf("ensure touchpoint: %w", err)
	}
	return nil
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
