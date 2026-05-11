package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type User struct {
	Sub   string
	Email string
	Name  string
}

type AuthError struct {
	Status  int
	Message string
}

func (e AuthError) Error() string {
	return e.Message
}

type K8sConfig struct {
	APIHost      string
	Allowlist    string
	OwnToken     string
	OwnTokenPath string
	CACertPath   string
	HTTPClient   *http.Client
}

type K8sAuthenticator struct {
	apiHost  string
	ownToken string
	allowed  map[string]struct{}
	client   *http.Client
}

func NewK8sAuthenticator(config K8sConfig) (*K8sAuthenticator, error) {
	allowed := AllowedServiceAccounts(config.Allowlist)
	if len(allowed) == 0 {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: "K8S_SA_ALLOWLIST not configured"}
	}

	ownToken := strings.TrimSpace(config.OwnToken)
	if ownToken == "" && config.OwnTokenPath != "" {
		data, err := os.ReadFile(config.OwnTokenPath)
		if err != nil {
			return nil, AuthError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("could not read pod SA token: %v", err)}
		}
		ownToken = strings.TrimSpace(string(data))
	}
	if ownToken == "" {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: "k8s SA token validation unavailable (not in-cluster)"}
	}

	client := config.HTTPClient
	if client == nil {
		var transport http.RoundTripper = http.DefaultTransport
		if config.CACertPath != "" {
			pool, err := certPool(config.CACertPath)
			if err != nil {
				return nil, err
			}
			transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		client = &http.Client{Transport: transport, Timeout: 10 * time.Second}
	}

	return &K8sAuthenticator{
		apiHost:  strings.TrimRight(config.APIHost, "/"),
		ownToken: ownToken,
		allowed:  allowed,
		client:   client,
	}, nil
}

func AllowedServiceAccounts(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" || !strings.Contains(entry, "/") {
			continue
		}
		parts := strings.SplitN(entry, "/", 2)
		namespace := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		if namespace == "" || name == "" {
			continue
		}
		out["system:serviceaccount:"+namespace+":"+name] = struct{}{}
	}
	return out
}

func LooksLikeK8sSAToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return false
	}
	_, ok := claims["kubernetes.io"].(map[string]any)
	return ok
}

func (a *K8sAuthenticator) RequireAdmin(ctx context.Context, token string) (User, error) {
	username, err := a.VerifyToken(ctx, token)
	if err != nil {
		return User{}, err
	}
	if _, ok := a.allowed[username]; !ok {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "service account not allowed: " + username}
	}
	return User{Sub: username, Email: username, Name: username}, nil
}

func (a *K8sAuthenticator) Resolve(ctx context.Context, token string) (User, bool, error) {
	username, err := a.VerifyToken(ctx, token)
	if err != nil {
		return User{}, false, err
	}
	_, isAdmin := a.allowed[username]
	return User{Sub: username, Email: username, Name: username}, isAdmin, nil
}

func (a *K8sAuthenticator) VerifyToken(ctx context.Context, token string) (string, error) {
	review := tokenReviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: token},
	}
	body, err := json.Marshal(review)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		a.apiHost+"/apis/authentication.k8s.io/v1/tokenreviews",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.ownToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", AuthError{Status: http.StatusServiceUnavailable, Message: "TokenReview unreachable: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return "", AuthError{Status: http.StatusServiceUnavailable, Message: "TokenReview not permitted; check glimmung RBAC"}
	}
	if resp.StatusCode >= 400 {
		return "", AuthError{Status: http.StatusUnauthorized, Message: fmt.Sprintf("TokenReview error: %d", resp.StatusCode)}
	}

	var parsed tokenReviewResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", AuthError{Status: http.StatusUnauthorized, Message: "TokenReview returned invalid JSON"}
	}
	if !parsed.Status.Authenticated {
		message := parsed.Status.Error
		if message == "" {
			message = "token rejected by TokenReview"
		}
		return "", AuthError{Status: http.StatusUnauthorized, Message: "invalid SA token: " + message}
	}
	username := parsed.Status.User.Username
	if !strings.HasPrefix(username, "system:serviceaccount:") {
		return "", AuthError{Status: http.StatusForbidden, Message: "non-service-account principal: " + username}
	}
	return username, nil
}

func certPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("could not read k8s CA cert: %v", err)}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: "could not parse k8s CA cert"}
	}
	return pool, nil
}

type tokenReviewRequest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Spec       tokenReviewSpec `json:"spec"`
}

type tokenReviewSpec struct {
	Token string `json:"token"`
}

type tokenReviewResponse struct {
	Status tokenReviewStatus `json:"status"`
}

type tokenReviewStatus struct {
	Authenticated bool            `json:"authenticated"`
	Error         string          `json:"error"`
	User          tokenReviewUser `json:"user"`
}

type tokenReviewUser struct {
	Username string `json:"username"`
}
