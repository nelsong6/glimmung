package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrNotFound is returned when a GitHub resource is not found.
var ErrNotFound = fmt.Errorf("not found")

// Client is a GitHub App client that mints installation tokens and calls the GitHub API.
type Client struct {
	appID          string
	installationID string
	privateKey     *rsa.PrivateKey
	mu             sync.Mutex
	token          string
	expiresAt      time.Time
}

// New parses the RSA private key (PEM) and returns a ready Client.
func New(appID, installationID, privateKey string) (*Client, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	return &Client{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
	}, nil
}

func (c *Client) mintJWT() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    c.appID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return tok.SignedString(c.privateKey)
}

// InstallationToken returns a cached GitHub App installation token, refreshing when near expiry.
func (c *Client) InstallationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiresAt) > 5*time.Minute {
		return c.token, nil
	}
	appJWT, err := c.mintJWT()
	if err != nil {
		return "", fmt.Errorf("mint app JWT: %w", err)
	}
	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", c.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub access_tokens returned %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode access_tokens response: %w", err)
	}
	c.token = payload.Token
	c.expiresAt = payload.ExpiresAt
	return c.token, nil
}

// FetchFileContents fetches raw file bytes from a GitHub repo path at the given ref.
// Returns ErrNotFound when the file doesn't exist.
func (c *Client) FetchFileContents(ctx context.Context, repo, path, ref string) ([]byte, error) {
	token, err := c.InstallationToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("ref", ref)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3.raw")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub contents returned %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}
