package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// RunMutationStore handles state-mutating run operations.
type RunMutationStore interface {
	// AbortRunByID marks a run as aborted and releases associated locks.
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
	// ReadRunIDForNumber resolves an issue-scoped run ref to (runID, runRef).
	ReadRunIDForNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, string, error)
	// ReadRunIDForCallbackToken resolves a run callback token to (runID, project, runRef).
	ReadRunIDForCallbackToken(ctx context.Context, token string) (string, string, string, error)
	// RecordRunStarted stamps the workflow_run_id on the latest attempt of the given run.
	RecordRunStarted(ctx context.Context, project, runID string, req RunStartedRequest) (string, error)
}

// AbortRunResult is the response for run abort operations.
type AbortRunResult struct {
	State             string  `json:"state"`
	RunRef            string  `json:"run_ref"`
	RunNumber         *int    `json:"run_number"`
	RunDisplayNumber  *string `json:"run_display_number"`
	GHRunCancelled    *bool   `json:"gh_run_cancelled"`
	IssueLockReleased *bool   `json:"issue_lock_released"`
	PRLockReleased    *bool   `json:"pr_lock_released"`
}

// RunCallbackResult is the response for GHA run callback operations.
type RunCallbackResult struct {
	RunRef            string   `json:"run_ref"`
	Decision          *string  `json:"decision"`
	IssueLockReleased *bool    `json:"issue_lock_released"`
	PRLockReleased    *bool    `json:"pr_lock_released"`
	PhaseComplete     *bool    `json:"phase_complete,omitempty"`
	CompletedJobIDs   []string `json:"completed_job_ids,omitempty"`
	PendingJobIDs     []string `json:"pending_job_ids,omitempty"`
	FailedJobIDs      []string `json:"failed_job_ids,omitempty"`
}

// RunStartedRequest is the request body for /run-callbacks/{token}/started.
type RunStartedRequest struct {
	WorkflowRunID int64   `json:"workflow_run_id"`
	ValidationURL *string `json:"validation_url"`
}

// RunAbortedRequest is the request body for /run-callbacks/{token}/aborted.
type RunAbortedRequest struct {
	Reason string `json:"reason"`
}

// abortRunByNumber handles POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/abort
func abortRunByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutStore, ok := store.(RunMutationStore)
		if !ok || mutStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run mutation store not configured")
			return
		}
		project := r.PathValue("project")
		issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || issueNumber < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		runNumber := r.PathValue("run_number")
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			reason = "aborted_via_admin_api"
		}

		runID, _, err := mutStore.ReadRunIDForNumber(r.Context(), project, issueNumber, runNumber)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read run failed")
			return
		}

		result, err := mutStore.AbortRunByID(r.Context(), project, runID, reason)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "abort run failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// runStartedByCallbackToken handles POST /v1/run-callbacks/{callback_token}/started
func runStartedByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutStore, ok := store.(RunMutationStore)
		if !ok || mutStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run mutation store not configured")
			return
		}
		token := r.PathValue("callback_token")
		runID, project, _, err := mutStore.ReadRunIDForCallbackToken(r.Context(), token)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read run by callback token failed")
			return
		}

		var req RunStartedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}

		runRef, err := mutStore.RecordRunStarted(r.Context(), project, runID, req)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "record run started failed")
			return
		}
		writeJSON(w, http.StatusOK, RunCallbackResult{RunRef: runRef})
	}
}

// runAbortedByCallbackToken handles POST /v1/run-callbacks/{callback_token}/aborted
func runAbortedByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutStore, ok := store.(RunMutationStore)
		if !ok || mutStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run mutation store not configured")
			return
		}
		token := r.PathValue("callback_token")
		runID, project, _, err := mutStore.ReadRunIDForCallbackToken(r.Context(), token)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read run by callback token failed")
			return
		}

		var req RunAbortedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Reason == "" {
			req.Reason = "aborted_via_callback"
		}

		result, err := mutStore.AbortRunByID(r.Context(), project, runID, req.Reason)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "abort run failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// githubWebhook handles POST /v1/webhook/github - verifies HMAC signature and acknowledges.
func githubWebhook(settings Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "read body failed")
			return
		}

		if settings.GitHubWebhookSecret != "" {
			sig := r.Header.Get("x-hub-signature-256")
			if sig == "" {
				writeProblem(w, http.StatusUnauthorized, "missing x-hub-signature-256")
				return
			}
			mac := hmac.New(sha256.New, []byte(settings.GitHubWebhookSecret))
			mac.Write(body)
			expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(sig), []byte(expected)) {
				writeProblem(w, http.StatusUnauthorized, "invalid webhook signature")
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"status": "accepted",
			"event":  r.Header.Get("x-github-event"),
		})
	}
}
