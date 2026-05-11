package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const defaultJWKSURL = "https://login.microsoftonline.com/common/discovery/v2.0/keys"

var entraIssuerPattern = regexp.MustCompile(`^https://login\.microsoftonline\.com/.+/v2\.0$`)

type EntraConfig struct {
	JWKSURL       string
	Audiences     []string
	AllowedEmails string
	HTTPClient    *http.Client
}

type EntraAuthenticator struct {
	jwksURL       string
	audiences     []string
	allowedEmails map[string]struct{}
	client        *http.Client
}

func NewEntraAuthenticator(config EntraConfig) (*EntraAuthenticator, error) {
	audiences := compactStrings(config.Audiences)
	if len(audiences) == 0 {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: "ENTRA_CLIENT_ID not configured"}
	}
	allowed := AllowedEmails(config.AllowedEmails)
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	jwksURL := config.JWKSURL
	if jwksURL == "" {
		jwksURL = defaultJWKSURL
	}
	return &EntraAuthenticator{
		jwksURL:       jwksURL,
		audiences:     audiences,
		allowedEmails: allowed,
		client:        client,
	}, nil
}

func AllowedEmails(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		email := strings.ToLower(strings.TrimSpace(entry))
		if email != "" {
			out[email] = struct{}{}
		}
	}
	return out
}

func (a *EntraAuthenticator) RequireAdmin(ctx context.Context, token string) (User, error) {
	claims, err := a.verifyToken(ctx, token)
	if err != nil {
		return User{}, err
	}

	email := strings.ToLower(firstClaimString(claims, "email", "preferred_username"))
	if email == "" {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "token has no email or preferred_username claim"}
	}
	if len(a.allowedEmails) == 0 {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "ALLOWED_EMAILS not configured"}
	}
	if _, ok := a.allowedEmails[email]; !ok {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "email not allowed"}
	}

	return User{
		Sub:   firstClaimString(claims, "sub"),
		Email: email,
		Name:  firstClaimString(claims, "name"),
	}, nil
}

func (a *EntraAuthenticator) Resolve(ctx context.Context, token string) (User, bool, error) {
	claims, err := a.verifyToken(ctx, token)
	if err != nil {
		return User{}, false, err
	}
	email := strings.ToLower(firstClaimString(claims, "email", "preferred_username"))
	if email == "" {
		return User{}, false, AuthError{Status: http.StatusUnauthorized, Message: "token has no email or preferred_username claim"}
	}
	_, isAdmin := a.allowedEmails[email]
	return User{
		Sub:   firstClaimString(claims, "sub"),
		Email: email,
		Name:  firstClaimString(claims, "name"),
	}, isAdmin, nil
}

func (a *EntraAuthenticator) verifyToken(ctx context.Context, token string) (jwt.MapClaims, error) {
	keys, err := a.fetchKeys(ctx)
	if err != nil {
		return nil, err
	}

	claims := jwt.MapClaims{}
	parser := jwt.NewParser(jwt.WithAudience(a.audiences...))
	parsed, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodRS256 {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		key := keys[kid]
		if key == nil {
			return nil, fmt.Errorf("unknown signing key: %s", kid)
		}
		return key, nil
	})
	if err != nil || !parsed.Valid {
		return nil, AuthError{Status: http.StatusUnauthorized, Message: "invalid token: " + errString(err)}
	}

	issuer := firstClaimString(claims, "iss")
	if !entraIssuerPattern.MatchString(issuer) {
		return nil, AuthError{Status: http.StatusUnauthorized, Message: "unexpected issuer: " + issuer}
	}
	return claims, nil
}

func (a *EntraAuthenticator) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: "JWKS unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("JWKS error: %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed jwksResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, AuthError{Status: http.StatusServiceUnavailable, Message: "JWKS returned invalid JSON"}
	}
	keys := map[string]*rsa.PublicKey{}
	for _, key := range parsed.Keys {
		if key.Kty != "RSA" {
			continue
		}
		publicKey, err := key.publicKey()
		if err != nil {
			return nil, err
		}
		keys[key.Kid] = publicKey
	}
	return keys, nil
}

type jwksResponse struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwk) publicKey() (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	exponent := 0
	for _, b := range eBytes {
		exponent = exponent<<8 + int(b)
	}
	if exponent == 0 {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponent}, nil
}

func firstClaimString(claims jwt.MapClaims, names ...string) string {
	for _, name := range names {
		if value, ok := claims[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return "token invalid"
	}
	return err.Error()
}
