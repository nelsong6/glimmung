package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ManagedOriginReconciler keeps auth.romaine.life's managed_origin table
// in sync with each project's slot wildcard. The wildcard is derived
// mechanically from `native_standby_dns.record_base` — there's no per-
// project wildcard config; the slot DNS shape is the single source of
// truth. Opt-in is `managed_auth_origins.enabled` in project metadata.
//
// Trigger model is event-only (no polling):
//   - `scaleProjectTestEnvironments` → ReconcileManagedOrigins
//   - `registerProject` / project metadata update → ReconcileManagedOrigins
//   - project deregister → DeleteManagedOrigins
//
// See nelsong6/glimmung#142 stage 2 for the cross-repo architecture.
const (
	ManagedAuthOriginStatusOK      = "ok"
	ManagedAuthOriginStatusSkipped = "skipped"
	ManagedAuthOriginStatusFailed  = "failed"
)

type ManagedOriginReconciler interface {
	ReconcileManagedOrigins(ctx context.Context, project Project) (ManagedAuthOriginStatus, error)
	DeleteManagedOrigins(ctx context.Context, projectName string) error
}

type ProjectManagedAuthOriginStatusWriter interface {
	SetProjectManagedAuthOriginStatus(ctx context.Context, project string, status ManagedAuthOriginStatus) (Project, error)
}

// ManagedAuthOriginStatus is persisted on the project's metadata under
// `managed_auth_origins_status` after each reconciliation, mirroring the
// shape used for `native_standby_workload_identity_status`.
type ManagedAuthOriginStatus struct {
	State            string   `json:"state"`
	Endpoint         string   `json:"endpoint,omitempty"`
	ManagedWildcards []string `json:"managed_wildcards"`
	LastReconciledAt string   `json:"last_reconciled_at,omitempty"`
	LastError        *string  `json:"last_error,omitempty"`
}

// ManagedOriginService reconciles by calling auth.romaine.life's
// /api/admin/origins/{project} admin surface. AuthN is the inbound
// caller's projected k8s SA token — glimmung's chart mounts a token
// with audience `https://auth.romaine.life` so it cannot be replayed
// against another JWT validator.
type ManagedOriginService struct {
	HTTPClient              *http.Client
	AuthBaseURL             string
	ServiceAccountTokenPath string
	Now                     func() time.Time
}

func (s ManagedOriginService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s ManagedOriginService) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s ManagedOriginService) ReconcileManagedOrigins(ctx context.Context, project Project) (ManagedAuthOriginStatus, error) {
	enabled, recordBase, projectKey := managedAuthOriginsFromProject(project)
	now := s.now().UTC().Format(time.RFC3339Nano)
	status := ManagedAuthOriginStatus{
		Endpoint:         s.AuthBaseURL,
		LastReconciledAt: now,
		ManagedWildcards: []string{},
	}
	if !enabled {
		status.State = ManagedAuthOriginStatusSkipped
		return status, nil
	}
	if recordBase == "" {
		err := errors.New("managed_auth_origins.enabled requires native_standby_dns.record_base")
		return s.failed(status, err), err
	}
	if projectKey == "" {
		err := errors.New("project name/id required to address auth admin endpoint")
		return s.failed(status, err), err
	}
	if strings.TrimSpace(s.AuthBaseURL) == "" {
		err := errors.New("auth base URL not configured")
		return s.failed(status, err), err
	}

	wildcard := "https://*." + strings.TrimSpace(recordBase)
	status.ManagedWildcards = []string{wildcard}

	body, err := json.Marshal(map[string]any{"wildcards": []string{wildcard}})
	if err != nil {
		return s.failed(status, err), err
	}
	url := s.adminURL(projectKey)
	if err := s.doWithRetry(ctx, http.MethodPut, url, body); err != nil {
		return s.failed(status, err), err
	}
	status.State = ManagedAuthOriginStatusOK
	return status, nil
}

func (s ManagedOriginService) DeleteManagedOrigins(ctx context.Context, projectName string) error {
	if strings.TrimSpace(s.AuthBaseURL) == "" {
		return errors.New("auth base URL not configured")
	}
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return errors.New("project name required")
	}
	return s.doWithRetry(ctx, http.MethodDelete, s.adminURL(projectName), nil)
}

func (s ManagedOriginService) failed(status ManagedAuthOriginStatus, err error) ManagedAuthOriginStatus {
	status.State = ManagedAuthOriginStatusFailed
	msg := err.Error()
	status.LastError = &msg
	return status
}

func (s ManagedOriginService) adminURL(projectKey string) string {
	return strings.TrimRight(s.AuthBaseURL, "/") + "/api/admin/origins/" + projectKey
}

// doWithRetry handles transient failures with bounded backoff inside a
// single trigger. 5xx and network errors retry; 4xx is a contract failure
// and surfaces immediately. The caller persists the resulting status so
// failure is visible; a subsequent trigger (next scale call or project
// update) re-runs reconciliation and self-heals.
func (s ManagedOriginService) doWithRetry(ctx context.Context, method, url string, body []byte) error {
	var lastErr error
	backoffs := []time.Duration{0, 250 * time.Millisecond, 1 * time.Second}
	for _, delay := range backoffs {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err := s.doOnce(ctx, method, url, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableAdminError(err) {
			return err
		}
	}
	return lastErr
}

type adminAPIError struct {
	status int
	body   string
}

func (e adminAPIError) Error() string {
	return fmt.Sprintf("auth admin %d: %s", e.status, e.body)
}

func isRetryableAdminError(err error) bool {
	var hse adminAPIError
	if errors.As(err, &hse) {
		return hse.status >= 500
	}
	// Network and context errors (other than canceled) are retryable.
	return !errors.Is(err, context.Canceled)
}

func (s ManagedOriginService) doOnce(ctx context.Context, method, url string, body []byte) error {
	token, err := s.readToken()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return adminAPIError{status: resp.StatusCode, body: strings.TrimSpace(string(respBody))}
	}
	// Drain body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (s ManagedOriginService) readToken() (string, error) {
	path := strings.TrimSpace(s.ServiceAccountTokenPath)
	if path == "" {
		return "", errors.New("service account token path not configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read SA token at %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// managedAuthOriginsFromProject extracts the opt-in flag, derives the slot
// wildcard base (`native_standby_dns.record_base`), and resolves the project
// key to use in the auth admin URL.
//
// Both snake_case (canonical) and camelCase (legacy) metadata keys are
// accepted to match the rest of glimmung's Project metadata handling.
func managedAuthOriginsFromProject(project Project) (enabled bool, recordBase, projectKey string) {
	cfg, ok := mapFromMap(project.Metadata, "managed_auth_origins")
	if !ok {
		cfg, ok = mapFromMap(project.Metadata, "managedAuthOrigins")
	}
	if !ok || !boolFromMap(cfg, "enabled") {
		return false, "", ""
	}
	enabled = true

	standby, ok := mapFromMap(project.Metadata, "native_standby_dns")
	if !ok {
		standby, _ = mapFromMap(project.Metadata, "nativeStandbyDns")
	}
	recordBase = firstNonEmpty(
		stringMapValue(standby, "record_base"),
		stringMapValue(standby, "recordBase"),
	)
	projectKey = firstNonEmpty(project.Name, project.ID)
	return enabled, recordBase, projectKey
}
