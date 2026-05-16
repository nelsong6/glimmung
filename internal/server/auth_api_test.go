package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeAuthResolver struct {
	user    auth.User
	isAdmin bool
	ok      bool
}

func (r fakeAuthResolver) ResolveCaller(context.Context, *http.Request) (auth.User, bool, bool) {
	return r.user, r.isAdmin, r.ok
}

func TestAuthMeUnsignedWhenNoResolver(t *testing.T) {
	var body AuthMeResponse
	getAuthMe(t, New(Settings{}), "", &body)

	if body.SignedIn || body.IsAdmin {
		t.Fatalf("body=%#v, want unsigned non-admin", body)
	}
}

func TestAuthMeUnsignedWhenTokenInvalid(t *testing.T) {
	var body AuthMeResponse
	getAuthMe(t, NewWithDependencies(Settings{}, nil, fakeAuthResolver{}), "Bearer bad", &body)

	if body.SignedIn || body.IsAdmin {
		t.Fatalf("body=%#v, want unsigned non-admin", body)
	}
}

func TestAuthMeSignedInAdmin(t *testing.T) {
	resolver := fakeAuthResolver{
		user:    auth.User{Sub: "sub", Email: "person@example.com", Name: "Person"},
		isAdmin: true,
		ok:      true,
	}

	var body AuthMeResponse
	getAuthMe(t, NewWithDependencies(Settings{}, nil, resolver), "Bearer token", &body)

	if !body.SignedIn || !body.IsAdmin || body.Email != "person@example.com" || body.Sub != "sub" {
		t.Fatalf("body=%#v", body)
	}
}

func getAuthMe(t *testing.T, handler http.Handler, authorization string, target *AuthMeResponse) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}
