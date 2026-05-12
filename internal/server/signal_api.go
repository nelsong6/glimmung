package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SignalStore interface {
	EnqueueSignal(ctx context.Context, req SignalEnqueue) (PublicSignal, error)
}

type SignalEnqueue struct {
	TargetType string
	TargetRepo string
	TargetRef  string
	Source     string
	Payload    map[string]any
}

type PublicSignal struct {
	Ref        string    `json:"ref"`
	TargetType string    `json:"target_type"`
	TargetRepo string    `json:"target_repo"`
	TargetRef  string    `json:"target_ref"`
	Source     string    `json:"source"`
	State      string    `json:"state"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

type SignalEnqueueRequest struct {
	TargetType string         `json:"target_type"`
	TargetRepo string         `json:"target_repo"`
	TargetRef  string         `json:"target_ref"`
	Source     string         `json:"source"`
	Payload    map[string]any `json:"payload"`
}

var validSignalTargetTypes = map[string]bool{"pr": true, "issue": true, "run": true}
var validSignalSources = map[string]bool{
	"gh_review": true, "gh_review_comment": true, "gh_comment": true,
	"glimmung_ui": true, "scheduled": true, "system": true,
}

func createSignal(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sigStore, ok := store.(SignalStore)
		if !ok || sigStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "signal store not configured")
			return
		}
		var body SignalEnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !validSignalTargetTypes[body.TargetType] {
			writeProblem(w, http.StatusBadRequest, "target_type must be pr, issue, or run")
			return
		}
		if strings.TrimSpace(body.TargetRepo) == "" {
			writeProblem(w, http.StatusBadRequest, "target_repo required")
			return
		}
		if strings.TrimSpace(body.TargetRef) == "" {
			writeProblem(w, http.StatusBadRequest, "target_ref required")
			return
		}
		if body.TargetType == "pr" && !validPRSignalTarget(body.TargetRepo, body.TargetRef) {
			writeProblem(w, http.StatusBadRequest, "pr signals require target_repo as owner/repo and target_ref as the PR number")
			return
		}
		source := firstNonEmpty(body.Source, "glimmung_ui")
		if !validSignalSources[source] {
			writeProblem(w, http.StatusBadRequest, "invalid source")
			return
		}
		req := SignalEnqueue{
			TargetType: body.TargetType,
			TargetRepo: body.TargetRepo,
			TargetRef:  body.TargetRef,
			Source:     source,
			Payload:    body.Payload,
		}
		sig, err := sigStore.EnqueueSignal(r.Context(), req)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusUnprocessableEntity, "signal target not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "enqueue signal failed")
			return
		}
		writeJSON(w, http.StatusOK, sig)
	}
}

func validPRSignalTarget(repo, ref string) bool {
	if !strings.Contains(repo, "/") {
		return false
	}
	pr, err := strconv.Atoi(strings.TrimSpace(ref))
	return err == nil && pr > 0
}
