package server

import (
	"context"
	"encoding/json"
	"errors"
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

func TestReadyzRequiresReadStore(t *testing.T) {
	handler := New(Settings{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["detail"] != "read store not configured" {
		t.Fatalf("body=%#v, want read store not configured", body)
	}
}

func TestReadyzChecksReadStore(t *testing.T) {
	handler := NewWithStore(Settings{}, readyzStore{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyzRejectsUnreadableStore(t *testing.T) {
	handler := NewWithStore(Settings{}, readyzErrorStore{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["detail"] != "read store not ready" {
		t.Fatalf("body=%#v, want read store not ready", body)
	}
}

func TestReadyzRecoversStorePanic(t *testing.T) {
	handler := NewWithStore(Settings{}, readyzPanicStore{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusServiceUnavailable)
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

func TestPublicConfigShipsGrafanaFields(t *testing.T) {
	body := requestConfig(t, Settings{
		TankOperatorBaseURL:   "https://tank.romaine.life",
		GrafanaBaseURL:        "https://grafana.romaine.life/",
		GrafanaLokiDatasource: "loki",
		NativeRunnerNamespace: "glimmung-runs",
	})

	if body["grafana_base_url"] != "https://grafana.romaine.life" {
		t.Fatalf("grafana_base_url=%q", body["grafana_base_url"])
	}
	if body["grafana_loki_datasource"] != "loki" {
		t.Fatalf("grafana_loki_datasource=%q", body["grafana_loki_datasource"])
	}
	if body["native_runner_namespace"] != "glimmung-runs" {
		t.Fatalf("native_runner_namespace=%q", body["native_runner_namespace"])
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

type readyzStore struct{}

func (readyzStore) ListProjects(context.Context) ([]Project, error) {
	return []Project{{Name: "glimmung"}}, nil
}

func (readyzStore) ListWorkflows(context.Context) ([]Workflow, error) {
	return nil, nil
}

type readyzErrorStore struct {
	readyzStore
}

func (readyzErrorStore) ListProjects(context.Context) ([]Project, error) {
	return nil, errors.New("projects store not configured")
}

type readyzPanicStore struct {
	readyzStore
}

func (readyzPanicStore) ListProjects(context.Context) ([]Project, error) {
	panic("typed nil store")
}
