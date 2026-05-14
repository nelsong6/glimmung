package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type RunStore interface {
	ListProjectRuns(ctx context.Context, project string, limit int) ([]RunReport, error)
	GetRunReportByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (RunReport, error)
}

type RunReportAttempt struct {
	AttemptIndex       int                       `json:"attempt_index"`
	Phase              string                    `json:"phase"`
	PhaseKind          string                    `json:"phase_kind"`
	WorkflowFilename   string                    `json:"workflow_filename"`
	DispatchedAt       time.Time                 `json:"dispatched_at"`
	CompletedAt        *time.Time                `json:"completed_at"`
	Conclusion         *string                   `json:"conclusion"`
	VerificationStatus *string                   `json:"verification_status"`
	EvidenceRefs       []string                  `json:"evidence_refs"`
	SummaryMarkdown    *string                   `json:"summary_markdown"`
	Decision           *string                   `json:"decision"`
	CostUSD            *float64                  `json:"cost_usd"`
	LogArchiveURL      *string                   `json:"log_archive_url"`
	SkippedFromRunRef  *string                   `json:"skipped_from_run_ref"`
	PhaseOutputs       map[string]string         `json:"phase_outputs"`
	JobCompletions     []RunAttemptJobCompletion `json:"job_completions"`
}

type RunAttemptJobCompletion struct {
	JobID               string            `json:"job_id"`
	CompletedAt         *time.Time        `json:"completed_at"`
	Conclusion          string            `json:"conclusion"`
	VerificationStatus  *string           `json:"verification_status"`
	VerificationReasons []string          `json:"verification_reasons"`
	CostUSD             float64           `json:"cost_usd"`
	PhaseOutputs        map[string]string `json:"phase_outputs"`
}

type RunReport struct {
	ID                  string             `json:"-"`
	Ref                 string             `json:"ref"`
	Project             string             `json:"project"`
	RunRef              string             `json:"run_ref"`
	RunNumber           *int               `json:"run_number"`
	RunDisplayNumber    *string            `json:"run_display_number"`
	ParentRunRef        *string            `json:"parent_run_ref"`
	RootRunRef          *string            `json:"root_run_ref"`
	OriginKind          *string            `json:"origin_kind"`
	EntrypointPhase     *string            `json:"entrypoint_phase"`
	IsCycle             bool               `json:"is_cycle"`
	CycleNumber         *int               `json:"cycle_number"`
	Workflow            string             `json:"workflow"`
	IssueRef            *string            `json:"issue_ref"`
	IssueRepo           *string            `json:"issue_repo"`
	IssueNumber         *int               `json:"issue_number"`
	State               string             `json:"state"`
	CurrentPhase        *string            `json:"current_phase"`
	AttemptsCount       int                `json:"attempts_count"`
	CumulativeCostUSD   float64            `json:"cumulative_cost_usd"`
	ValidationURL       *string            `json:"validation_url"`
	ScreenshotsMarkdown *string            `json:"screenshots_markdown"`
	AbortReason         *string            `json:"abort_reason"`
	StartedAt           time.Time          `json:"started_at"`
	CompletedAt         *time.Time         `json:"completed_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	Attempts            []RunReportAttempt `json:"attempts"`
}

func listProjectRuns(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runStore, ok := store.(RunStore)
		if !ok || runStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		limit, ok := parseRunLimit(w, r)
		if !ok {
			return
		}
		rows, err := runStore.ListProjectRuns(r.Context(), r.PathValue("project"), limit)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list project runs failed")
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func getRunReportByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runStore, ok := store.(RunStore)
		if !ok || runStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || issueNumber < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		report, err := runStore.GetRunReportByNumber(
			r.Context(),
			r.PathValue("project"),
			issueNumber,
			r.PathValue("run_number"),
		)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "get run report failed")
			return
		}
		writeJSON(w, http.StatusOK, report)
	}
}

func parseRunLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 100, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		writeProblem(w, http.StatusBadRequest, "limit must be between 1 and 500")
		return 0, false
	}
	return limit, true
}
