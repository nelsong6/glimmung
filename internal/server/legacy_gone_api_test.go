package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyStorageIDRoutesReturnGone(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	for _, tc := range []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/v1/issues/by-id/glimmung/01KISSUE", "Issue storage-ID lookup is disabled"},
		{http.MethodPatch, "/v1/issues/by-id/glimmung/01KISSUE", "Issue storage-ID mutation is disabled"},
		{http.MethodPost, "/v1/issues/by-id/glimmung/01KISSUE/archive", "Issue storage-ID mutation is disabled"},
		{http.MethodPost, "/v1/issues/by-id/glimmung/01KISSUE/discard", "Issue storage-ID mutation is disabled"},
		{http.MethodPost, "/v1/issues/by-id/glimmung/01KISSUE/comments", "Issue storage-ID comments are disabled"},
		{http.MethodPatch, "/v1/issues/by-id/glimmung/01KISSUE/comments/comment-1", "Issue storage-ID comments are disabled"},
		{http.MethodDelete, "/v1/issues/by-id/glimmung/01KISSUE/comments/comment-1", "Issue storage-ID comments are disabled"},
		{http.MethodGet, "/v1/reports/by-id/glimmung/report-1", "touchpoints are no longer addressable by storage id"},
		{http.MethodGet, "/v1/touchpoints/by-id/glimmung/report-1", "touchpoints are no longer addressable by storage id"},
		{http.MethodGet, "/v1/reports/by-id/glimmung/report-1/versions", "touchpoint versions are no longer addressable by storage id"},
		{http.MethodGet, "/v1/touchpoints/by-id/glimmung/report-1/versions", "touchpoint versions are no longer addressable by storage id"},
		{http.MethodGet, "/v1/reports/by-id/glimmung/report-1/versions/1", "touchpoint versions are no longer addressable by storage id"},
		{http.MethodGet, "/v1/touchpoints/by-id/glimmung/report-1/versions/1", "touchpoint versions are no longer addressable by storage id"},
		{http.MethodPost, "/v1/reports/by-id/glimmung/report-1/versions", "touchpoint versions are no longer addressable by storage id"},
		{http.MethodPost, "/v1/touchpoints/by-id/glimmung/report-1/versions", "touchpoint versions are no longer addressable by storage id"},
		{http.MethodPatch, "/v1/reports/by-id/glimmung/report-1", "touchpoints are no longer patchable by storage id"},
		{http.MethodPatch, "/v1/touchpoints/by-id/glimmung/report-1", "touchpoints are no longer patchable by storage id"},
		{http.MethodGet, "/v1/projects/glimmung/issues/455/runs/1/native/pod-logs", "native pod log proxying is retired"},
		{http.MethodPost, "/v1/projects/glimmung/issues/455/runs/1/native/completed", "native completion by run coordinates is retired"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusGone {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body=%s missing %q", rec.Body.String(), tc.want)
			}
		})
	}
}
