package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteInternalErrorPreservesUnderlyingErrorInLog is the load-bearing
// test for glimmung#514: a 5xx written by writeInternalError must surface
// the underlying error string in the slog.Error record (the actual diagnosis
// signal), even though the response body keeps the abstract `summary` (the
// public API contract).
//
// The bug we are guarding against: prior to glimmung#514, the only path to
// write a 5xx was `writeProblem(w, http.StatusInternalServerError, "summary")`,
// which discarded `err` at the call site. The Cosmos / IO / panic-recovery
// exception was unrecoverable from logs because nothing in middleware or
// recovery saw it — the handler threw it away before either ran. See
// docs/quality-timeframes.md: "Observability exists for the bugs a user
// would otherwise have to guess about."
func TestWriteInternalErrorPreservesUnderlyingErrorInLog(t *testing.T) {
	var buf bytes.Buffer
	captureHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	orig := slog.Default()
	slog.SetDefault(slog.New(captureHandler))
	t.Cleanup(func() { slog.SetDefault(orig) })

	cosmosErr := errors.New("cosmos cross-partition order by failed: request rate too large")
	req := httptest.NewRequest(http.MethodGet, "/v1/touchpoints", nil)
	rec := httptest.NewRecorder()

	writeInternalError(rec, req, cosmosErr, "list touchpoints failed")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["detail"] != "list touchpoints failed" {
		t.Errorf("body[detail] = %q, want %q", body["detail"], "list touchpoints failed")
	}

	logged := buf.String()
	for _, wantSubstring := range []string{
		"list touchpoints failed",                        // the summary
		"cosmos cross-partition order by failed",         // the underlying err — the whole point
		`"method":"GET"`,                                 // request method
		`"route":"(unmatched)"`,                          // r.Pattern is empty outside ServeMux; fallback fires
		`"level":"ERROR"`,                                // emitted at ERROR level for alerting
	} {
		if !strings.Contains(logged, wantSubstring) {
			t.Errorf("log missing %q\nlog output: %s", wantSubstring, logged)
		}
	}
}

// TestWriteInternalErrorUsesRequestPattern verifies the helper prefers the
// ServeMux's registered route pattern (r.Pattern, Go 1.22+) over a raw URL
// path. Cardinality of the slog `route` attribute is intentionally bounded
// to the set of registered patterns — raw URLs would leak path params (issue
// numbers, project slugs) into log indices.
func TestWriteInternalErrorUsesRequestPattern(t *testing.T) {
	var buf bytes.Buffer
	captureHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	orig := slog.Default()
	slog.SetDefault(slog.New(captureHandler))
	t.Cleanup(func() { slog.SetDefault(orig) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/touchpoint", func(w http.ResponseWriter, r *http.Request) {
		writeInternalError(w, r, errors.New("upstream cosmos timeout"), "get touchpoint failed")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/projects/tank-operator/issues/42/touchpoint", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	logged := buf.String()
	wantRoute := `"route":"GET /v1/projects/{project}/issues/{issue_number}/touchpoint"`
	if !strings.Contains(logged, wantRoute) {
		t.Errorf("log missing %q (raw path leaked into route?)\nlog output: %s", wantRoute, logged)
	}
	// Path params must NOT appear in the route attribute — that would
	// blow up Loki/Prom cardinality.
	for _, leak := range []string{`"route":"/v1/projects/tank-operator`, "tank-operator/issues/42"} {
		if strings.Contains(logged, leak) {
			t.Errorf("route leaked path param: contained %q\nlog output: %s", leak, logged)
		}
	}
}
