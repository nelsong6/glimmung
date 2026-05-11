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

func TestPublicConfigUsesProductionClientForProductionHost(t *testing.T) {
	body := requestConfig(t, "glimmung.romaine.life", "", Settings{
		EntraClientID:       "prod-client",
		EntraTestClientID:   "test-client",
		TankOperatorBaseURL: "https://tank.romaine.life/",
	})

	if body["entra_client_id"] != "prod-client" {
		t.Fatalf("entra_client_id=%q, want prod-client", body["entra_client_id"])
	}
	if body["authority"] != defaultAuthority {
		t.Fatalf("authority=%q, want %q", body["authority"], defaultAuthority)
	}
	if body["tank_operator_base_url"] != "https://tank.romaine.life" {
		t.Fatalf("tank_operator_base_url=%q", body["tank_operator_base_url"])
	}
}

func TestPublicConfigUsesTestClientForDisposableHost(t *testing.T) {
	body := requestConfig(t, "preview.glimmung.dev.romaine.life", "", Settings{
		EntraClientID:       "prod-client",
		EntraTestClientID:   "test-client",
		TankOperatorBaseURL: "https://tank.romaine.life",
	})

	if body["entra_client_id"] != "test-client" {
		t.Fatalf("entra_client_id=%q, want test-client", body["entra_client_id"])
	}
}

func TestPublicConfigUsesForwardedHost(t *testing.T) {
	body := requestConfig(t, "glimmung.romaine.life", "preview.glimmung.dev.romaine.life, proxy", Settings{
		EntraClientID:       "prod-client",
		EntraTestClientID:   "test-client",
		TankOperatorBaseURL: "https://tank.romaine.life",
	})

	if body["entra_client_id"] != "test-client" {
		t.Fatalf("entra_client_id=%q, want test-client", body["entra_client_id"])
	}
}

func TestFrontendClientSelectionFallsBackWithoutTestClient(t *testing.T) {
	body := requestConfig(t, "preview.glimmung.dev.romaine.life", "", Settings{
		EntraClientID:       "prod-client",
		TankOperatorBaseURL: "https://tank.romaine.life",
	})

	if body["entra_client_id"] != "prod-client" {
		t.Fatalf("entra_client_id=%q, want prod-client", body["entra_client_id"])
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

func requestConfig(t *testing.T, host string, forwardedHost string, settings Settings) map[string]string {
	t.Helper()

	handler := New(settings)
	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	req.Host = host
	if forwardedHost != "" {
		req.Header.Set("x-forwarded-host", forwardedHost)
	}
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
