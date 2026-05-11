package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
