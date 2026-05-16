package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthz(t *testing.T) {
	handler := New(Settings{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body=%#v, want status ok", body)
	}
}

func TestPublicConfigPointsAtAuthRomaineLife(t *testing.T) {
	body := requestConfig(t, Settings{
		TankOperatorBaseURL: "https://tank.romaine.life/",
	})

	if body["auth_url"] != defaultAuthURL {
		t.Fatalf("auth_url=%q, want %q", body["auth_url"], defaultAuthURL)
	}
	if body["tank_operator_base_url"] != "https://tank.romaine.life" {
		t.Fatalf("tank_operator_base_url=%q", body["tank_operator_base_url"])
	}
}

func TestStaticServingReturnsIndexAndAssets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "index.html"), "<main>app</main>")
	writeFile(t, filepath.Join(root, "assets", "app.js"), "console.log('ok')")

	handler := New(Settings{StaticDir: root})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<main>app</main>" {
		t.Fatalf("index response status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('ok')" {
		t.Fatalf("asset response status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestStaticServingFallsBackToIndexForSPARoutes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "index.html"), "<main>app</main>")

	handler := New(Settings{StaticDir: root})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/issues/123", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "<main>app</main>" {
		t.Fatalf("spa response status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestStaticOverrideWins(t *testing.T) {
	base := t.TempDir()
	override := t.TempDir()
	writeFile(t, filepath.Join(base, "index.html"), "base")
	writeFile(t, filepath.Join(override, "index.html"), "override")

	handler := New(Settings{StaticDir: base, StaticOverrideDir: override})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "override" {
		t.Fatalf("override response status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestStaticAssetRejectsMissingAndTraversal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "index.html"), "<main>app</main>")
	writeFile(t, filepath.Join(root, "assets", "app.js"), "console.log('ok')")
	writeFile(t, filepath.Join(root, "secret.txt"), "secret")

	handler := New(Settings{StaticDir: root})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status=%d, want 404", rec.Code)
	}

	if found, ok := staticFile(staticRoots(Settings{StaticDir: root}), "assets", "..", "secret.txt"); ok {
		t.Fatalf("traversal resolved to %q", found)
	}
}

func requestConfig(t *testing.T, settings Settings) map[string]string {
	t.Helper()

	handler := New(settings)
	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}
