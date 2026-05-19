package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const (
	testLeaseProjectDefaultTTLSecondsKey       = "test_lease_default_ttl_seconds"
	testLeaseProjectDefaultTTLSecondsLegacyKey = "testLeaseDefaultTTLSeconds"
)

type TestLeaseDefaults struct {
	GlobalTTLSeconds int `json:"global_ttl_seconds"`
}

type TestLeaseDefaultTTLReader interface {
	ReadTestLeaseDefaults(ctx context.Context) (TestLeaseDefaults, error)
}

type TestLeaseDefaultTTLWriter interface {
	TestLeaseDefaultTTLReader
	SetGlobalTestLeaseDefaultTTL(ctx context.Context, ttlSeconds *int) (TestLeaseDefaults, error)
	SetProjectTestLeaseDefaultTTL(ctx context.Context, project string, ttlSeconds *int) (Project, error)
}

type testLeaseDefaultTTLUpdateRequest struct {
	Project    *string `json:"project"`
	TTLSeconds *int    `json:"ttl_seconds"`
	Reset      bool    `json:"reset"`
}

type testLeaseDefaultTTLUpdateResult struct {
	Defaults TestLeaseDefaults `json:"defaults"`
	Project  *Project          `json:"project,omitempty"`
}

func updateTestLeaseDefaultTTL(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(TestLeaseDefaultTTLWriter)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test lease default TTL store not configured")
			return
		}

		var req testLeaseDefaultTTLUpdateRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		ttlSeconds, ok := requestedDefaultTTL(w, req)
		if !ok {
			return
		}
		project := ""
		if req.Project != nil {
			project = strings.TrimSpace(*req.Project)
		}

		if project == "" {
			defaults, err := writer.SetGlobalTestLeaseDefaultTTL(r.Context(), ttlSeconds)
			if err != nil {
				writeTestLeaseDefaultTTLError(w, r, err, "update global test lease default TTL failed")
				return
			}
			writeJSON(w, http.StatusOK, testLeaseDefaultTTLUpdateResult{
				Defaults: normalizeTestLeaseDefaults(defaults),
			})
			return
		}

		updated, err := writer.SetProjectTestLeaseDefaultTTL(r.Context(), project, ttlSeconds)
		if err != nil {
			writeTestLeaseDefaultTTLError(w, r, err, "update project test lease default TTL failed")
			return
		}
		defaults := readTestLeaseDefaultsOrFallback(r.Context(), store)
		writeJSON(w, http.StatusOK, testLeaseDefaultTTLUpdateResult{
			Defaults: defaults,
			Project:  &updated,
		})
	}
}

func requestedDefaultTTL(w http.ResponseWriter, req testLeaseDefaultTTLUpdateRequest) (*int, bool) {
	if req.Reset {
		return nil, true
	}
	if req.TTLSeconds == nil {
		writeProblem(w, http.StatusUnprocessableEntity, "ttl_seconds is required unless reset is true")
		return nil, false
	}
	if *req.TTLSeconds <= 0 {
		writeProblem(w, http.StatusUnprocessableEntity, "ttl_seconds must be positive")
		return nil, false
	}
	value := *req.TTLSeconds
	return &value, true
}

func writeTestLeaseDefaultTTLError(w http.ResponseWriter, r *http.Request, err error, summary string) {
	var validationErr ValidationError
	switch {
	case errors.As(err, &validationErr):
		writeProblem(w, http.StatusUnprocessableEntity, validationErr.Message)
	case errors.Is(err, ErrNotFound):
		writeProblem(w, http.StatusNotFound, "project not found")
	default:
		writeInternalError(w, r, err, summary)
	}
}

func defaultTTLForGeneratedTestLease(ctx context.Context, store ReadStore, project Project) int {
	if ttl, ok := projectTestLeaseDefaultTTL(project); ok {
		return ttl
	}
	return readTestLeaseDefaultsOrFallback(ctx, store).GlobalTTLSeconds
}

func readTestLeaseDefaultsOrFallback(ctx context.Context, store ReadStore) TestLeaseDefaults {
	reader, ok := store.(TestLeaseDefaultTTLReader)
	if !ok || reader == nil {
		return TestLeaseDefaults{GlobalTTLSeconds: testSlotDefaultTTLSeconds}
	}
	defaults, err := reader.ReadTestLeaseDefaults(ctx)
	if err != nil {
		return TestLeaseDefaults{GlobalTTLSeconds: testSlotDefaultTTLSeconds}
	}
	return normalizeTestLeaseDefaults(defaults)
}

func normalizeTestLeaseDefaults(defaults TestLeaseDefaults) TestLeaseDefaults {
	if defaults.GlobalTTLSeconds <= 0 {
		defaults.GlobalTTLSeconds = testSlotDefaultTTLSeconds
	}
	return defaults
}

func projectTestLeaseDefaultTTL(project Project) (int, bool) {
	if ttl, ok := positiveIntFromMap(project.Metadata, testLeaseProjectDefaultTTLSecondsKey); ok {
		return ttl, true
	}
	if ttl, ok := positiveIntFromMap(project.Metadata, testLeaseProjectDefaultTTLSecondsLegacyKey); ok {
		return ttl, true
	}
	return 0, false
}
