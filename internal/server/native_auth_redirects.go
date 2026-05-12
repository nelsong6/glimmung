package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	NativeAuthRedirectStatusOK      = "ok"
	NativeAuthRedirectStatusSkipped = "skipped"
	NativeAuthRedirectStatusFailed  = "failed"
)

type ApplicationRedirectRef struct {
	ObjectID string
	ClientID string
}

type ApplicationRedirectApp struct {
	ObjectID     string
	ClientID     string
	RedirectURIs []string
}

type ApplicationRedirectClient interface {
	ReadApplicationRedirects(ctx context.Context, ref ApplicationRedirectRef) (ApplicationRedirectApp, error)
	UpdateApplicationRedirects(ctx context.Context, objectID string, redirectURIs []string) error
}

type NativeAuthRedirectReconciler interface {
	ReconcileNativeAuthRedirects(ctx context.Context, project Project) (NativeAuthRedirectStatus, error)
}

type ProjectNativeAuthRedirectStatusWriter interface {
	SetProjectNativeAuthRedirectStatus(ctx context.Context, project string, status NativeAuthRedirectStatus) (Project, error)
}

type NativeAuthRedirectStatus struct {
	State               string   `json:"state"`
	Provider            string   `json:"provider,omitempty"`
	ApplicationObjectID string   `json:"application_object_id,omitempty"`
	ApplicationClientID string   `json:"application_client_id,omitempty"`
	DesiredCount        int      `json:"desired_count"`
	ManagedRedirectURIs []string `json:"managed_redirect_uris"`
	AddedRedirectURIs   []string `json:"added_redirect_uris,omitempty"`
	RemovedRedirectURIs []string `json:"removed_redirect_uris,omitempty"`
	LastReconciledAt    string   `json:"last_reconciled_at,omitempty"`
	LastError           *string  `json:"last_error,omitempty"`
}

type NativeAuthRedirectService struct {
	Client ApplicationRedirectClient
	Now    func() time.Time
}

type nativeAuthRedirectConfig struct {
	Enabled                bool
	Provider               string
	ApplicationObjectID    string
	ApplicationClientID    string
	RedirectURIMode        string
	ProductionRedirectURIs []string
	ExtraRedirectURIs      []string
	RecordBase             string
	SlotPrefix             string
	Count                  int
}

func (s NativeAuthRedirectService) ReconcileNativeAuthRedirects(ctx context.Context, project Project) (NativeAuthRedirectStatus, error) {
	cfg, ok, err := nativeAuthRedirectConfigFromProject(project)
	if !ok {
		return NativeAuthRedirectStatus{}, nil
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	status := NativeAuthRedirectStatus{
		State:               NativeAuthRedirectStatusFailed,
		Provider:            cfg.Provider,
		ApplicationObjectID: cfg.ApplicationObjectID,
		ApplicationClientID: cfg.ApplicationClientID,
		DesiredCount:        cfg.Count,
		ManagedRedirectURIs: desiredManagedRedirectURIs(cfg),
		LastReconciledAt:    now,
	}
	if err != nil {
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	if s.Client == nil {
		err := errors.New("native auth redirect client not configured")
		status.LastError = stringPtr(err.Error())
		return status, err
	}

	app, err := s.Client.ReadApplicationRedirects(ctx, ApplicationRedirectRef{
		ObjectID: cfg.ApplicationObjectID,
		ClientID: cfg.ApplicationClientID,
	})
	if err != nil {
		err = fmt.Errorf("read app redirects: %w", err)
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	status.ApplicationObjectID = firstNonEmpty(app.ObjectID, cfg.ApplicationObjectID)
	status.ApplicationClientID = firstNonEmpty(app.ClientID, cfg.ApplicationClientID)

	desired, added, removed := reconcileRedirectURISet(app.RedirectURIs, cfg)
	if len(added) > 0 || len(removed) > 0 {
		if err := s.Client.UpdateApplicationRedirects(ctx, app.ObjectID, desired); err != nil {
			err = fmt.Errorf("update app redirects: %w", err)
			status.LastError = stringPtr(err.Error())
			return status, err
		}
	}

	status.State = NativeAuthRedirectStatusOK
	status.AddedRedirectURIs = added
	status.RemovedRedirectURIs = removed
	status.LastError = nil
	return status, nil
}

func (s NativeAuthRedirectService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func nativeAuthRedirectConfigFromProject(project Project) (nativeAuthRedirectConfig, bool, error) {
	cfgMap, ok := mapFromMap(project.Metadata, "native_auth_redirects")
	if !ok {
		cfgMap, ok = mapFromMap(project.Metadata, "nativeAuthRedirects")
	}
	if !ok || !boolFromMap(cfgMap, "enabled") {
		return nativeAuthRedirectConfig{}, false, nil
	}

	standby, ok := mapFromMap(project.Metadata, "native_standby_dns")
	if !ok {
		standby, ok = mapFromMap(project.Metadata, "nativeStandbyDns")
	}
	cfg := nativeAuthRedirectConfig{
		Enabled:                true,
		Provider:               firstNonEmpty(stringMapValue(cfgMap, "provider"), "entra"),
		ApplicationObjectID:    firstNonEmpty(stringMapValue(cfgMap, "application_object_id"), stringMapValue(cfgMap, "applicationObjectId"), stringMapValue(cfgMap, "app_registration_object_id"), stringMapValue(cfgMap, "entra_application_object_id")),
		ApplicationClientID:    firstNonEmpty(stringMapValue(cfgMap, "application_client_id"), stringMapValue(cfgMap, "applicationClientId"), stringMapValue(cfgMap, "client_id"), stringMapValue(cfgMap, "entra_client_id")),
		RedirectURIMode:        firstNonEmpty(stringMapValue(cfgMap, "redirect_uri_mode"), stringMapValue(cfgMap, "redirectUriMode"), "spa"),
		ProductionRedirectURIs: stringSliceFromMap(cfgMap, "production_redirect_uris", "productionRedirectUris"),
		ExtraRedirectURIs:      stringSliceFromMap(cfgMap, "extra_redirect_uris", "extraRedirectUris"),
		RecordBase:             firstNonEmpty(stringMapValue(standby, "record_base"), stringMapValue(standby, "recordBase")),
		SlotPrefix:             firstNonEmpty(stringMapValue(standby, "slot_prefix"), stringMapValue(standby, "slotPrefix")),
		Count:                  nonNegativeIntMapValue(standby, "count"),
	}
	switch cfg.Provider {
	case "", "entra":
		cfg.Provider = "entra"
	default:
		return cfg, true, fmt.Errorf("unsupported native auth redirect provider %q", cfg.Provider)
	}
	if cfg.ApplicationObjectID == "" && cfg.ApplicationClientID == "" {
		return cfg, true, errors.New("native_auth_redirects requires application_object_id or application_client_id")
	}
	if cfg.RedirectURIMode != "spa" {
		return cfg, true, fmt.Errorf("unsupported native auth redirect_uri_mode %q", cfg.RedirectURIMode)
	}
	if !ok {
		return cfg, true, errors.New("native_standby_dns metadata is required")
	}
	if cfg.RecordBase == "" {
		return cfg, true, errors.New("native_standby_dns.record_base is required")
	}
	if cfg.SlotPrefix == "" {
		return cfg, true, errors.New("native_standby_dns.slot_prefix is required")
	}
	return cfg, true, nil
}

func desiredManagedRedirectURIs(cfg nativeAuthRedirectConfig) []string {
	uris := make([]string, 0, cfg.Count)
	for i := 1; i <= cfg.Count; i++ {
		uris = append(uris, fmt.Sprintf("https://%s-%d.%s/", cfg.SlotPrefix, i, cfg.RecordBase))
	}
	return uris
}

func reconcileRedirectURISet(current []string, cfg nativeAuthRedirectConfig) (desired []string, added []string, removed []string) {
	desiredManaged := desiredManagedRedirectURIs(cfg)
	desiredSet := map[string]bool{}
	currentSet := map[string]bool{}
	for _, uri := range current {
		trimmed := strings.TrimSpace(uri)
		if trimmed == "" {
			continue
		}
		currentSet[trimmed] = true
		if idx, ok := managedSlotRedirectIndex(trimmed, cfg); ok && idx > cfg.Count {
			removed = append(removed, trimmed)
			continue
		}
		desiredSet[trimmed] = true
	}
	for _, uri := range append(append([]string{}, cfg.ProductionRedirectURIs...), cfg.ExtraRedirectURIs...) {
		trimmed := strings.TrimSpace(uri)
		if trimmed != "" {
			desiredSet[trimmed] = true
		}
	}
	for _, uri := range desiredManaged {
		desiredSet[uri] = true
	}
	for uri := range desiredSet {
		desired = append(desired, uri)
		if !currentSet[uri] {
			added = append(added, uri)
		}
	}
	sort.Strings(desired)
	sort.Strings(added)
	sort.Strings(removed)
	return desired, added, removed
}

func managedSlotRedirectIndex(raw string, cfg nativeAuthRedirectConfig) (int, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" {
		return 0, false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return 0, false
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, false
	}
	host := strings.ToLower(parsed.Host)
	suffix := "." + strings.ToLower(cfg.RecordBase)
	if !strings.HasSuffix(host, suffix) {
		return 0, false
	}
	slot := strings.TrimSuffix(host, suffix)
	prefix := strings.ToLower(cfg.SlotPrefix) + "-"
	if !strings.HasPrefix(slot, prefix) {
		return 0, false
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(slot, prefix))
	if err != nil || idx < 1 {
		return 0, false
	}
	return idx, true
}

func stringMapValue(values map[string]any, key string) string {
	value, ok := stringFromMap(values, key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func stringSliceFromMap(values map[string]any, keys ...string) []string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case []string:
			out := make([]string, 0, len(typed))
			for _, value := range typed {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					out = append(out, trimmed)
				}
			}
			return out
		case []any:
			out := make([]string, 0, len(typed))
			for _, value := range typed {
				if s, ok := value.(string); ok {
					if trimmed := strings.TrimSpace(s); trimmed != "" {
						out = append(out, trimmed)
					}
				}
			}
			return out
		}
	}
	return nil
}

func nonNegativeIntMapValue(values map[string]any, key string) int {
	if values == nil {
		return 0
	}
	raw, ok := values[key]
	if !ok {
		return 0
	}
	switch typed := raw.(type) {
	case int:
		if typed > 0 {
			return typed
		}
	case int64:
		if typed > 0 {
			return int(typed)
		}
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case string:
		parsed, err := strconv.Atoi(typed)
		if err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}
