package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

type NativeGitHubTokenMinter interface {
	InstallationToken(ctx context.Context) (string, error)
}

func positivePathInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	value, err := strconv.Atoi(r.PathValue(name))
	if err != nil || value < 1 {
		writeProblem(w, http.StatusBadRequest, name+" must be a positive integer")
		return 0, false
	}
	return value, true
}

type NativeGitHubTokenResult struct {
	Repo  string `json:"repo"`
	Token string `json:"token"`
}

type runNumberResolver interface {
	ReadRunByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, error)
}

func nativeGitHubTokenByCallbackToken(store ReadStore, minter NativeGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "GitHub token minter not configured")
			return
		}
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
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
		writeNativeGitHubToken(w, r, completionStore, minter, project, runID)
	}
}

func nativeGitHubTokenByNumber(store ReadStore, minter NativeGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "GitHub token minter not configured")
			return
		}
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		runResolver, ok := store.(runNumberResolver)
		if !ok || runResolver == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run lookup store not configured")
			return
		}
		issueNumber, ok := positivePathInt(w, r, "issue_number")
		if !ok {
			return
		}
		runID, err := runResolver.ReadRunByNumber(r.Context(), r.PathValue("project"), issueNumber, r.PathValue("run_number"))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run failed")
			return
		}
		writeNativeGitHubToken(w, r, completionStore, minter, r.PathValue("project"), runID)
	}
}

func writeNativeGitHubToken(w http.ResponseWriter, r *http.Request, store RunCompletionStore, minter NativeGitHubTokenMinter, project, runID string) {
	run, err := store.ReadRunForReplay(r.Context(), project, runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return
	}
	if run.IssueRepo == "" {
		writeProblem(w, http.StatusConflict, "run has no issue repo")
		return
	}
	token, err := minter.InstallationToken(r.Context())
	if err != nil {
		writeProblem(w, http.StatusBadGateway, "mint GitHub token failed")
		return
	}
	writeJSON(w, http.StatusOK, NativeGitHubTokenResult{Repo: run.IssueRepo, Token: token})
}
