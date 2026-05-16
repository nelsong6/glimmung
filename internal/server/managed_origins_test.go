package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests use a fake auth admin server to avoid network calls. The reconciler
// reads its SA token from disk; we point it at a tempfile.

type recordedRequest struct {
	method string
	path   string
	auth   string
	body   string
}

func newFakeAdminServer(t *testing.T, handler func(*recordedRequest) (int, string)) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var recorded []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			body:   strings.TrimSpace(string(body)),
		}
		recorded = append(recorded, req)
		status, response := handler(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv, &recorded
}

func writeTokenFile(t *testing.T, value string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func newTestProject(meta map[string]any) Project {
	return Project{
		ID:       "tank-operator",
		Name:     "tank-operator",
		Metadata: meta,
	}
}

func TestManagedOriginsReconcileSkipsWhenDisabled(t *testing.T) {
	service := ManagedOriginService{
		AuthBaseURL:             "https://auth.example",
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
		Now:                     func() time.Time { return time.Unix(0, 0).UTC() },
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{}))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status.State != ManagedAuthOriginStatusSkipped {
		t.Fatalf("state=%q want skipped", status.State)
	}
	if len(status.ManagedWildcards) != 0 {
		t.Fatalf("wildcards=%v want empty", status.ManagedWildcards)
	}
}

func TestManagedOriginsReconcileSkipsWhenEnabledFalse(t *testing.T) {
	service := ManagedOriginService{AuthBaseURL: "https://auth.example", ServiceAccountTokenPath: writeTokenFile(t, "tok")}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": false},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	}))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status.State != ManagedAuthOriginStatusSkipped {
		t.Fatalf("state=%q want skipped", status.State)
	}
}

func TestManagedOriginsReconcileDerivesWildcardFromRecordBase(t *testing.T) {
	srv, recorded := newFakeAdminServer(t, func(*recordedRequest) (int, string) { return 200, `{}` })
	service := ManagedOriginService{
		AuthBaseURL:             srv.URL,
		ServiceAccountTokenPath: writeTokenFile(t, "fake-token"),
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	}))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status.State != ManagedAuthOriginStatusOK {
		t.Fatalf("state=%q want ok (lastError=%v)", status.State, derefError(status.LastError))
	}
	if got, want := status.ManagedWildcards, []string{"https://*.tank.dev.romaine.life"}; !equalStringSlice(got, want) {
		t.Fatalf("wildcards=%v want %v", got, want)
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(*recorded))
	}
	got := (*recorded)[0]
	if got.method != http.MethodPut {
		t.Fatalf("method=%q want PUT", got.method)
	}
	if got.path != "/api/admin/origins/tank-operator" {
		t.Fatalf("path=%q", got.path)
	}
	if got.auth != "Bearer fake-token" {
		t.Fatalf("auth=%q want bearer fake-token", got.auth)
	}
	var payload struct {
		Wildcards []string `json:"wildcards"`
	}
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("body=%q: %v", got.body, err)
	}
	if !equalStringSlice(payload.Wildcards, []string{"https://*.tank.dev.romaine.life"}) {
		t.Fatalf("body wildcards=%v", payload.Wildcards)
	}
}

func TestManagedOriginsReconcileIsIdempotent(t *testing.T) {
	srv, recorded := newFakeAdminServer(t, func(*recordedRequest) (int, string) { return 200, `{}` })
	service := ManagedOriginService{
		AuthBaseURL:             srv.URL,
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	project := newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	})
	for i := 0; i < 3; i++ {
		if _, err := service.ReconcileManagedOrigins(context.Background(), project); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if got := len(*recorded); got != 3 {
		t.Fatalf("recorded=%d want 3 (idempotent body, but each call still hits the API)", got)
	}
	for i, r := range *recorded {
		if r.body != (*recorded)[0].body {
			t.Fatalf("iter %d body=%q differs from first %q", i, r.body, (*recorded)[0].body)
		}
	}
}

func TestManagedOriginsReconcileFailsWhenRecordBaseMissing(t *testing.T) {
	service := ManagedOriginService{
		AuthBaseURL:             "https://auth.example",
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if status.State != ManagedAuthOriginStatusFailed {
		t.Fatalf("state=%q want failed", status.State)
	}
	if status.LastError == nil || !strings.Contains(*status.LastError, "record_base") {
		t.Fatalf("lastError=%v", derefError(status.LastError))
	}
}

func TestManagedOriginsReconcileFailsAndRecordsErrorOn4xx(t *testing.T) {
	calls := 0
	srv, _ := newFakeAdminServer(t, func(*recordedRequest) (int, string) {
		calls++
		return 422, `{"error":"bad wildcard"}`
	})
	service := ManagedOriginService{
		AuthBaseURL:             srv.URL,
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if status.State != ManagedAuthOriginStatusFailed {
		t.Fatalf("state=%q want failed", status.State)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1 (4xx is non-retryable)", calls)
	}
	if status.LastError == nil || !strings.Contains(*status.LastError, "422") {
		t.Fatalf("lastError=%v", derefError(status.LastError))
	}
}

func TestManagedOriginsReconcileRetriesOn5xx(t *testing.T) {
	calls := 0
	srv, _ := newFakeAdminServer(t, func(*recordedRequest) (int, string) {
		calls++
		if calls < 3 {
			return 502, `{"error":"upstream"}`
		}
		return 200, `{}`
	})
	service := ManagedOriginService{
		AuthBaseURL:             srv.URL,
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	}))
	if err != nil {
		t.Fatalf("expected eventual success: %v", err)
	}
	if status.State != ManagedAuthOriginStatusOK {
		t.Fatalf("state=%q want ok", status.State)
	}
	if calls != 3 {
		t.Fatalf("calls=%d want 3 (two 502 then 200)", calls)
	}
}

func TestManagedOriginsReconcileExhaustsRetriesOn5xx(t *testing.T) {
	calls := 0
	srv, _ := newFakeAdminServer(t, func(*recordedRequest) (int, string) {
		calls++
		return 502, `upstream down`
	})
	service := ManagedOriginService{
		AuthBaseURL:             srv.URL,
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	}))
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if status.State != ManagedAuthOriginStatusFailed {
		t.Fatalf("state=%q want failed", status.State)
	}
	if calls != 3 {
		t.Fatalf("calls=%d want 3 retries", calls)
	}
}

func TestManagedOriginsDeleteCallsDelete(t *testing.T) {
	srv, recorded := newFakeAdminServer(t, func(*recordedRequest) (int, string) { return 200, `{}` })
	service := ManagedOriginService{
		AuthBaseURL:             srv.URL,
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	if err := service.DeleteManagedOrigins(context.Background(), "tank-operator"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded=%d want 1", len(*recorded))
	}
	got := (*recorded)[0]
	if got.method != http.MethodDelete {
		t.Fatalf("method=%q want DELETE", got.method)
	}
	if got.path != "/api/admin/origins/tank-operator" {
		t.Fatalf("path=%q", got.path)
	}
}

func TestManagedOriginsReconcileFailsWhenAuthBaseURLUnconfigured(t *testing.T) {
	service := ManagedOriginService{
		ServiceAccountTokenPath: writeTokenFile(t, "tok"),
	}
	status, err := service.ReconcileManagedOrigins(context.Background(), newTestProject(map[string]any{
		"managed_auth_origins": map[string]any{"enabled": true},
		"native_standby_dns":   map[string]any{"record_base": "tank.dev.romaine.life"},
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if status.State != ManagedAuthOriginStatusFailed {
		t.Fatalf("state=%q want failed", status.State)
	}
}

func TestIsRetryableAdminError(t *testing.T) {
	tests := []struct {
		err   error
		retry bool
	}{
		{adminAPIError{status: 502}, true},
		{adminAPIError{status: 503}, true},
		{adminAPIError{status: 422}, false},
		{adminAPIError{status: 401}, false},
		{errors.New("connection refused"), true},
		{context.Canceled, false},
	}
	for _, tc := range tests {
		if got := isRetryableAdminError(tc.err); got != tc.retry {
			t.Errorf("err=%v retry=%v want %v", tc.err, got, tc.retry)
		}
	}
}

func derefError(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
