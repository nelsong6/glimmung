package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAllowedServiceAccounts(t *testing.T) {
	allowed := AllowedServiceAccounts(" tank-operator/tank-operator, bad, ns/sa ")

	for _, username := range []string{
		"system:serviceaccount:tank-operator:tank-operator",
		"system:serviceaccount:ns:sa",
	} {
		if _, ok := allowed[username]; !ok {
			t.Fatalf("%s not in allowlist %#v", username, allowed)
		}
	}
	if _, ok := allowed["bad"]; ok {
		t.Fatalf("malformed entry should be ignored: %#v", allowed)
	}
}

func TestLooksLikeK8sSAToken(t *testing.T) {
	if !LooksLikeK8sSAToken(jwtWithClaims(t, map[string]any{"kubernetes.io": map[string]any{"namespace": "ns"}})) {
		t.Fatal("expected Kubernetes-shaped token")
	}
	if LooksLikeK8sSAToken(jwtWithClaims(t, map[string]any{"iss": "https://login.microsoftonline.com/x/v2.0"})) {
		t.Fatal("expected Entra-shaped token not to look like Kubernetes")
	}
	if LooksLikeK8sSAToken("not-a-jwt") {
		t.Fatal("expected malformed token to be rejected")
	}
}

func TestK8sRequireAdminAcceptsAllowedServiceAccount(t *testing.T) {
	tokenReview := newTokenReviewServer(t, http.StatusOK, tokenReviewResponse{
		Status: tokenReviewStatus{
			Authenticated: true,
			User:          tokenReviewUser{Username: "system:serviceaccount:ns:sa"},
		},
	})
	defer tokenReview.Close()

	authenticator := newTestAuthenticator(t, tokenReview.URL, "ns/sa")
	user, err := authenticator.RequireAdmin(context.Background(), "caller-token")
	if err != nil {
		t.Fatalf("RequireAdmin returned error: %v", err)
	}
	if user.Sub != "system:serviceaccount:ns:sa" {
		t.Fatalf("user=%#v", user)
	}
}

func TestK8sRequireAdminRejectsDisallowedServiceAccount(t *testing.T) {
	tokenReview := newTokenReviewServer(t, http.StatusOK, tokenReviewResponse{
		Status: tokenReviewStatus{
			Authenticated: true,
			User:          tokenReviewUser{Username: "system:serviceaccount:other:sa"},
		},
	})
	defer tokenReview.Close()

	authenticator := newTestAuthenticator(t, tokenReview.URL, "ns/sa")
	_, err := authenticator.RequireAdmin(context.Background(), "caller-token")
	assertAuthStatus(t, err, http.StatusForbidden, "not allowed")
}

func TestK8sVerifyTokenHandlesTokenReviewFailures(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     tokenReviewResponse
		wantCode int
		wantText string
	}{
		{
			name:     "rbac forbidden",
			status:   http.StatusForbidden,
			wantCode: http.StatusServiceUnavailable,
			wantText: "not permitted",
		},
		{
			name:     "token rejected",
			status:   http.StatusOK,
			body:     tokenReviewResponse{Status: tokenReviewStatus{Authenticated: false, Error: "bad token"}},
			wantCode: http.StatusUnauthorized,
			wantText: "bad token",
		},
		{
			name:     "non service account",
			status:   http.StatusOK,
			body:     tokenReviewResponse{Status: tokenReviewStatus{Authenticated: true, User: tokenReviewUser{Username: "alice"}}},
			wantCode: http.StatusForbidden,
			wantText: "non-service-account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenReview := newTokenReviewServer(t, tt.status, tt.body)
			defer tokenReview.Close()

			authenticator := newTestAuthenticator(t, tokenReview.URL, "ns/sa")
			_, err := authenticator.VerifyToken(context.Background(), "caller-token")
			assertAuthStatus(t, err, tt.wantCode, tt.wantText)
		})
	}
}

func TestK8sVerifyTokenReloadsOwnTokenFile(t *testing.T) {
	tokenPath := t.TempDir() + "/token"
	if err := os.WriteFile(tokenPath, []byte("own-token-1"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	var gotAuth []string
	tokenReview := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(tokenReviewResponse{
			Status: tokenReviewStatus{
				Authenticated: true,
				User:          tokenReviewUser{Username: "system:serviceaccount:ns:sa"},
			},
		})
	}))
	defer tokenReview.Close()

	authenticator, err := NewK8sAuthenticator(K8sConfig{
		APIHost:      tokenReview.URL,
		Allowlist:    "ns/sa",
		OwnTokenPath: tokenPath,
	})
	if err != nil {
		t.Fatalf("NewK8sAuthenticator returned error: %v", err)
	}
	if _, err := authenticator.VerifyToken(context.Background(), "caller-token"); err != nil {
		t.Fatalf("first VerifyToken returned error: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte("own-token-2"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if _, err := authenticator.VerifyToken(context.Background(), "caller-token"); err != nil {
		t.Fatalf("second VerifyToken returned error: %v", err)
	}

	if len(gotAuth) != 2 {
		t.Fatalf("TokenReview calls=%d, want 2", len(gotAuth))
	}
	if gotAuth[0] != "Bearer own-token-1" || gotAuth[1] != "Bearer own-token-2" {
		t.Fatalf("Authorization headers=%#v", gotAuth)
	}
}

func TestNewK8sAuthenticatorRequiresAllowlistAndOwnToken(t *testing.T) {
	_, err := NewK8sAuthenticator(K8sConfig{Allowlist: "", OwnToken: "own"})
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "ALLOWLIST")

	_, err = NewK8sAuthenticator(K8sConfig{Allowlist: "ns/sa"})
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "not in-cluster")
}

func newTokenReviewServer(t *testing.T, status int, body tokenReviewResponse) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer own-token" {
			t.Fatalf("Authorization=%q", got)
		}
		var review tokenReviewRequest
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			t.Fatalf("decode review: %v", err)
		}
		if review.Spec.Token == "" {
			t.Fatal("review token is empty")
		}

		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func newTestAuthenticator(t *testing.T, apiHost string, allowlist string) *K8sAuthenticator {
	t.Helper()

	authenticator, err := NewK8sAuthenticator(K8sConfig{
		APIHost:   apiHost,
		Allowlist: allowlist,
		OwnToken:  "own-token",
	})
	if err != nil {
		t.Fatalf("NewK8sAuthenticator returned error: %v", err)
	}
	return authenticator
}

func assertAuthStatus(t *testing.T, err error, status int, contains string) {
	t.Helper()

	authErr, ok := err.(AuthError)
	if !ok {
		t.Fatalf("error=%T %v, want AuthError", err, err)
	}
	if authErr.Status != status {
		t.Fatalf("status=%d, want %d", authErr.Status, status)
	}
	if contains != "" && !strings.Contains(authErr.Message, contains) {
		t.Fatalf("message=%q, want containing %q", authErr.Message, contains)
	}
}

func jwtWithClaims(t *testing.T, claims map[string]any) string {
	t.Helper()

	header, err := json.Marshal(map[string]any{"alg": "none"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(body) + ".sig"
}
