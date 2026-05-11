package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeAdminAuthenticator struct {
	user auth.User
	err  error
}

func (a fakeAdminAuthenticator) RequireAdmin(context.Context, string) (auth.User, error) {
	return a.user, a.err
}

func TestRequireAdminAllowsAdmin(t *testing.T) {
	handler := requireAdmin(
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", rec.Code)
	}
}

func TestRequireAdminFailsClosedWithoutAuthenticator(t *testing.T) {
	handler := requireAdmin(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestRequireAdminMapsAuthErrors(t *testing.T) {
	handler := requireAdmin(
		fakeAdminAuthenticator{err: auth.AuthError{Status: http.StatusForbidden, Message: "not allowed"}},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}
