package server

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
)

const (
	defaultPort                = "8000"
	defaultAuthority           = "https://login.microsoftonline.com/common"
	defaultTankOperatorBaseURL = "https://tank.romaine.life"
)

type Settings struct {
	Port                string
	EntraClientID       string
	EntraTestClientID   string
	TankOperatorBaseURL string
}

func SettingsFromEnv() Settings {
	return Settings{
		Port:                envOrDefault("PORT", defaultPort),
		EntraClientID:       os.Getenv("ENTRA_CLIENT_ID"),
		EntraTestClientID:   os.Getenv("ENTRA_TEST_CLIENT_ID"),
		TankOperatorBaseURL: envOrDefault("TANK_OPERATOR_BASE_URL", defaultTankOperatorBaseURL),
	}
}

func New(settings Settings) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /v1/config", publicConfig(settings))
	return mux
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func publicConfig(settings Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := frontendEntraClientID(settings, requestHost(r))
		writeJSON(w, http.StatusOK, map[string]string{
			"entra_client_id":        clientID,
			"authority":              defaultAuthority,
			"tank_operator_base_url": strings.TrimRight(settings.TankOperatorBaseURL, "/"),
		})
	}
}

func requestHost(r *http.Request) string {
	forwarded := r.Header.Get("x-forwarded-host")
	host := forwarded
	if comma := strings.Index(host, ","); comma >= 0 {
		host = host[:comma]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end >= 0 {
			return strings.ToLower(strings.TrimPrefix(host[:end], "["))
		}
	}
	if withoutPort, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(withoutPort)
	}
	if colon := strings.Index(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	return strings.ToLower(host)
}

func frontendEntraClientID(settings Settings, host string) string {
	if settings.EntraTestClientID != "" && isDisposableFrontendHost(host) {
		return settings.EntraTestClientID
	}
	return settings.EntraClientID
}

func isDisposableFrontendHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
	return host == "glimmung.dev.romaine.life" || strings.HasSuffix(host, ".glimmung.dev.romaine.life")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func envOrDefault(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}
