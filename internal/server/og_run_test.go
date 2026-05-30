package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchRunURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		path      string
		wantOK    bool
		wantProj  string
		wantIssue int
		wantSlug  string
	}{
		{
			name:      "issue-scoped run",
			path:      "/projects/glimmung/issues/141/runs/3",
			wantOK:    true,
			wantProj:  "glimmung",
			wantIssue: 141,
			wantSlug:  "3",
		},
		{
			name:      "issue-scoped run with cycle suffix",
			path:      "/projects/glimmung/issues/141/runs/3/cycles/2",
			wantOK:    true,
			wantProj:  "glimmung",
			wantIssue: 141,
			wantSlug:  "3",
		},
		{
			name:      "escaped project name",
			path:      "/projects/tank-operator/issues/22/runs/1",
			wantOK:    true,
			wantProj:  "tank-operator",
			wantIssue: 22,
			wantSlug:  "1",
		},
		{
			name:   "project-scoped (no issue) is not enriched",
			path:   "/projects/glimmung/runs/abc-123",
			wantOK: false,
		},
		{
			name:   "unrelated path",
			path:   "/projects/glimmung/issues/141",
			wantOK: false,
		},
		{
			name:   "root",
			path:   "/",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := matchRunURL(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v (match=%+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.project != tc.wantProj || got.issueNumber != tc.wantIssue || got.runSlug != tc.wantSlug {
				t.Fatalf("got=%+v want project=%q issue=%d slug=%q", got, tc.wantProj, tc.wantIssue, tc.wantSlug)
			}
		})
	}
}

func TestInjectOGTags(t *testing.T) {
	t.Parallel()
	html := []byte(`<!doctype html><html><head><title>glimmung</title></head><body></body></html>`)
	tags := `<meta property="og:title" content="run">` + "\n"
	out := injectOGTags(html, tags)
	if !strings.Contains(string(out), `<title>glimmung</title>`+"\n"+tags) {
		t.Fatalf("tags not inserted after title: %s", out)
	}

	// No title, head only:
	headOnly := []byte(`<html><head></head><body></body></html>`)
	out = injectOGTags(headOnly, tags)
	if !strings.Contains(string(out), `<head>`+"\n"+tags) {
		t.Fatalf("tags not inserted after head: %s", out)
	}

	// No head at all: returns input unchanged.
	noHead := []byte(`<html><body></body></html>`)
	out = injectOGTags(noHead, tags)
	if string(out) != string(noHead) {
		t.Fatalf("unexpected mutation: %s", out)
	}
}

// TestRetiredOGSVGRouteStaysDeleted is the migration guard for the
// retired SVG OG renderer. Per .tank/docs/migration-policy.md the old
// /og/runs/.../{slug}.svg surface is deleted end to end. A live route
// that silently serves a different format would be the "fallback /
// compatibility" smell the policy rejects, so the assertion here is:
// the SVG URL returns 404 and the response body is not SVG.
func TestRetiredOGSVGRouteStaysDeleted(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	store := &fakeRunStore{rows: []RunReport{{
		Project:     "glimmung",
		Workflow:    "default",
		IssueNumber: intPtr(141),
		State:       "passed",
		StartedAt:   now,
		UpdatedAt:   now,
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/og/runs/glimmung/141/3.svg", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("retired SVG URL must 404, got status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.HasPrefix(rec.Body.String(), "<svg ") {
		t.Fatalf("retired SVG URL must not serve SVG; body=%s", rec.Body.String())
	}
}

func TestRunOGImagePNGRendersPNG(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	store := &fakeRunStore{rows: []RunReport{{
		Project:           "glimmung",
		Workflow:          "default",
		IssueNumber:       intPtr(141),
		State:             "in_progress",
		CurrentPhase:      stringPtr("implement"),
		AttemptsCount:     2,
		CumulativeCostUSD: 1.23,
		StartedAt:         now,
		UpdatedAt:         now,
		PhaseExecutions: []RunPhaseExecution{
			{Name: "plan", Kind: "plan", State: "completed"},
			{Name: "implement", Kind: "code", State: "in_progress"},
			{Name: "verify", Kind: "verify", State: "pending"},
		},
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/og/runs/glimmung/141/3.png", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content-type=%q", got)
	}
	body := rec.Body.Bytes()
	// PNG magic: 0x89 'P' 'N' 'G' 0x0d 0x0a 0x1a 0x0a
	pngSig := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if len(body) < len(pngSig) || !bytes.Equal(body[:len(pngSig)], pngSig) {
		t.Fatalf("body does not start with PNG signature; first 8 bytes = % x", body[:min(8, len(body))])
	}
	if store.runNumber != "3" {
		t.Fatalf("run number=%q", store.runNumber)
	}
}

func TestRunOGImagePNGNotFound(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeRunStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/og/runs/glimmung/141/3.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServeSPAWithOGInjectsRunTags(t *testing.T) {
	dir := t.TempDir()
	indexHTML := `<!doctype html><html><head><title>glimmung</title></head><body><div id="root"></div></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	store := &fakeRunStore{rows: []RunReport{{
		Project:       "glimmung",
		Workflow:      "default",
		IssueNumber:   intPtr(141),
		State:         "passed",
		AttemptsCount: 1,
		StartedAt:     now,
		UpdatedAt:     now,
		PhaseExecutions: []RunPhaseExecution{
			{Name: "plan", State: "completed"},
		},
	}}}
	handler := NewWithStore(Settings{StaticDir: dir}, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/glimmung/issues/141/runs/1", nil)
	req.Host = "glimmung.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	wantSnippets := []string{
		`<meta property="og:type" content="article">`,
		`<meta property="og:title" content="default · glimmung #141 · run 1">`,
		// og:image points at the PNG variant so Discord and other
		// strictly-raster unfurlers render the picture.
		`<meta property="og:image" content="https://glimmung.example/og/runs/glimmung/141/1.png">`,
		`<meta property="og:image:type" content="image/png">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<meta name="twitter:image" content="https://glimmung.example/og/runs/glimmung/141/1.png">`,
	}
	for _, snip := range wantSnippets {
		if !strings.Contains(body, snip) {
			t.Errorf("missing snippet %q in body=%s", snip, body)
		}
	}
}

func TestServeSPAWithOGFallsBackForNonRun(t *testing.T) {
	dir := t.TempDir()
	indexHTML := `<!doctype html><html><head><title>glimmung</title></head><body></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &fakeRunStore{}
	handler := NewWithStore(Settings{StaticDir: dir}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/playbooks", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "og:image") {
		t.Fatalf("non-run path should not include og:image: %s", rec.Body.String())
	}
}

func TestServeSPAWithOGFallsBackOnRunLookupError(t *testing.T) {
	dir := t.TempDir()
	indexHTML := `<!doctype html><html><head><title>glimmung</title></head><body></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty fakeRunStore returns ErrNotFound for unknown rows.
	store := &fakeRunStore{}
	handler := NewWithStore(Settings{StaticDir: dir}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects/glimmung/issues/999/runs/1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "og:image") {
		t.Fatalf("unknown run should not include og:image: %s", rec.Body.String())
	}
}
