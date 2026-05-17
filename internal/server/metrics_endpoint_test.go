package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMetricsEndpointMounted is the wire-level guarantee that /metrics is
// exposed alongside /healthz. If this fails, scrapers and the
// ServiceMonitor template have nothing to point at.
//
// We drive one request through the server first so the HTTP middleware
// records a sample — Prometheus omits metric families with no observations,
// so this also confirms the middleware → registry path is wired end to end.
func TestMetricsEndpointMounted(t *testing.T) {
	handler := New(Settings{})

	// Prime the HTTP counter with one /healthz request.
	primeReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	primeRec := httptest.NewRecorder()
	handler.ServeHTTP(primeRec, primeReq)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status=%d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if !strings.Contains(string(body), "glimmung_http_requests_total") {
		t.Errorf("/metrics body missing glimmung_http_requests_total; got %d bytes starting with: %s", len(body), preview(body, 200))
	}
}

// TestRequestsThroughServerRecordHTTPMetrics confirms the middleware is
// in the request path — every request through the mux moves the counter.
// Without this guard a refactor that drops the wrapper would silently
// kill HTTP observability.
func TestRequestsThroughServerRecordHTTPMetrics(t *testing.T) {
	handler := New(Settings{})

	// Drive a known route — /healthz is wired in newHandlerWithReconcilers
	// regardless of dependencies, so this works without a store.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz status=%d on iteration %d", rec.Code, i)
		}
	}

	// Scrape and verify a sample appeared.
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	handler.ServeHTTP(metricsRec, metricsReq)
	body, _ := io.ReadAll(metricsRec.Body)
	// The pattern label uses the Go 1.22+ ServeMux registered pattern.
	if !strings.Contains(string(body), `glimmung_http_requests_total{method="GET",pattern="GET /healthz",status="200"}`) {
		t.Errorf("expected glimmung_http_requests_total sample for GET /healthz; body did not contain it")
	}
}

func preview(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
