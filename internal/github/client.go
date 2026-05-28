package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

type PullRequestEnsureRequest struct {
	Repo  string
	Base  string
	Head  string
	Title string
	Body  string
}

type PullRequest struct {
	Number  int
	Title   string
	Body    string
	Branch  string
	BaseRef string
	HeadSHA string
	HTMLURL string
	State   string
}

func (c *Client) EnsurePullRequest(ctx context.Context, req PullRequestEnsureRequest) (PullRequest, error) {
	req.Repo = strings.TrimSpace(req.Repo)
	req.Base = firstNonEmpty(strings.TrimSpace(req.Base), "main")
	req.Head = strings.TrimSpace(req.Head)
	req.Title = strings.TrimSpace(req.Title)
	if req.Repo == "" || req.Head == "" || req.Title == "" {
		return PullRequest{}, fmt.Errorf("repo, head, and title are required")
	}
	if existing, ok, err := c.findOpenPullRequest(ctx, req.Repo, req.Head); err != nil {
		return PullRequest{}, err
	} else if ok {
		return existing, nil
	}
	created, err := c.createPullRequest(ctx, req)
	if err == nil {
		return created, nil
	}
	if !strings.Contains(err.Error(), "422") {
		return PullRequest{}, err
	}
	if existing, ok, findErr := c.findOpenPullRequest(ctx, req.Repo, req.Head); findErr != nil {
		return PullRequest{}, err
	} else if ok {
		return existing, nil
	}
	return PullRequest{}, err
}

// PullRequestMergeRequest carries the parameters for an idempotent merge.
type PullRequestMergeRequest struct {
	Repo        string
	Number      int
	CommitTitle string
	MergeMethod string // "merge" | "squash" | "rebase"; defaults to "merge".
}

// PullRequestMergeResult records the outcome of an attempt at merging.
//
// AlreadyMerged is true when the PR was already merged on GitHub when the
// request arrived; the operation is a no-op success in that case.
type PullRequestMergeResult struct {
	Number        int
	HTMLURL       string
	State         string
	MergeCommitSHA string
	AlreadyMerged bool
}

// MergePullRequest idempotently merges the target PR.
//
// The flow:
//  1. GET /repos/{repo}/pulls/{number} — if pull.merged is true, return
//     AlreadyMerged=true. Treat that as a successful no-op.
//  2. PUT /repos/{repo}/pulls/{number}/merge — perform the merge.
//
// GitHub returns 405 Method Not Allowed when a PR isn't mergeable (open
// blocking review, failing required check, conflicts). The error is bubbled
// up so the caller can decide whether to retry or surface the reason.
func (c *Client) MergePullRequest(ctx context.Context, req PullRequestMergeRequest) (PullRequestMergeResult, error) {
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		return PullRequestMergeResult{}, fmt.Errorf("repo is required")
	}
	if req.Number < 1 {
		return PullRequestMergeResult{}, fmt.Errorf("pull request number must be a positive integer")
	}
	method := strings.TrimSpace(strings.ToLower(req.MergeMethod))
	switch method {
	case "", "merge":
		method = "merge"
	case "squash", "rebase":
	default:
		return PullRequestMergeResult{}, fmt.Errorf("merge_method must be one of merge, squash, rebase; got %q", req.MergeMethod)
	}

	// 1. Idempotency probe: is the PR already merged?
	getURL := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", req.Repo, req.Number)
	var current githubPullRequestDetail
	if err := c.githubJSON(ctx, http.MethodGet, getURL, nil, &current); err != nil {
		return PullRequestMergeResult{}, fmt.Errorf("read pull request: %w", err)
	}
	if current.Merged {
		return PullRequestMergeResult{
			Number:         current.Number,
			HTMLURL:        current.HTMLURL,
			State:          current.State,
			MergeCommitSHA: current.MergeCommitSHA,
			AlreadyMerged:  true,
		}, nil
	}

	// 2. Perform the merge.
	mergeURL := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/merge", req.Repo, req.Number)
	body := map[string]any{
		"merge_method": method,
	}
	if title := strings.TrimSpace(req.CommitTitle); title != "" {
		body["commit_title"] = title
	}
	var resp githubMergeResponse
	if err := c.githubJSON(ctx, http.MethodPut, mergeURL, body, &resp); err != nil {
		return PullRequestMergeResult{}, fmt.Errorf("merge pull request: %w", err)
	}
	if !resp.Merged {
		return PullRequestMergeResult{}, fmt.Errorf("GitHub merge returned merged=false: %s", strings.TrimSpace(resp.Message))
	}
	return PullRequestMergeResult{
		Number:         req.Number,
		HTMLURL:        current.HTMLURL,
		State:          "closed",
		MergeCommitSHA: resp.SHA,
		AlreadyMerged:  false,
	}, nil
}

func (c *Client) findOpenPullRequest(ctx context.Context, repo, head string) (PullRequest, bool, error) {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || owner == "" {
		return PullRequest{}, false, fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	values := url.Values{}
	values.Set("state", "open")
	values.Set("head", owner+":"+head)
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/pulls?%s", repo, values.Encode())
	var pulls []githubPullRequest
	if err := c.githubJSON(ctx, http.MethodGet, apiURL, nil, &pulls); err != nil {
		return PullRequest{}, false, err
	}
	if len(pulls) == 0 {
		return PullRequest{}, false, nil
	}
	return pullRequestFromGitHub(pulls[0]), true, nil
}

func (c *Client) createPullRequest(ctx context.Context, req PullRequestEnsureRequest) (PullRequest, error) {
	body := map[string]any{
		"title":                 req.Title,
		"head":                  req.Head,
		"base":                  req.Base,
		"body":                  req.Body,
		"maintainer_can_modify": true,
	}
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/pulls", req.Repo)
	var pr githubPullRequest
	if err := c.githubJSON(ctx, http.MethodPost, apiURL, body, &pr); err != nil {
		return PullRequest{}, err
	}
	return pullRequestFromGitHub(pr), nil
}

func (c *Client) githubJSON(ctx context.Context, method, apiURL string, body any, out any) error {
	token, err := c.InstallationToken(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub %s %s returned %d: %s", method, apiURL, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

type githubPullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// githubPullRequestDetail extends githubPullRequest with the merged/merge_commit
// fields returned by GET /repos/{repo}/pulls/{number}; the list endpoint
// doesn't return them and the create endpoint returns merged=false always.
type githubPullRequestDetail struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
}

// githubMergeResponse is the response shape of PUT pulls/{number}/merge.
type githubMergeResponse struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

func pullRequestFromGitHub(pr githubPullRequest) PullRequest {
	return PullRequest{
		Number:  pr.Number,
		Title:   pr.Title,
		Body:    pr.Body,
		Branch:  pr.Head.Ref,
		BaseRef: pr.Base.Ref,
		HeadSHA: pr.Head.SHA,
		HTMLURL: pr.HTMLURL,
		State:   pr.State,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
