package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	NativeWorkloadIdentityStatusOK      = "ok"
	NativeWorkloadIdentityStatusSkipped = "skipped"
	NativeWorkloadIdentityStatusFailed  = "failed"

	defaultWorkloadIdentityAudience = "api://AzureADTokenExchange"
)

type FederatedIdentityCredentialRef struct {
	SubscriptionID string
	ResourceGroup  string
	IdentityName   string
	CredentialName string
}

type FederatedIdentityCredential struct {
	FederatedIdentityCredentialRef
	Issuer    string
	Subject   string
	Audiences []string
}

type FederatedIdentityCredentialClient interface {
	UpsertFederatedIdentityCredential(ctx context.Context, credential FederatedIdentityCredential) error
	ListFederatedIdentityCredentials(ctx context.Context, ref FederatedIdentityCredentialRef) ([]FederatedIdentityCredential, error)
	DeleteFederatedIdentityCredential(ctx context.Context, ref FederatedIdentityCredentialRef) error
}

type NativeWorkloadIdentityReconciler interface {
	ReconcileNativeWorkloadIdentities(ctx context.Context, project Project) (NativeWorkloadIdentityStatus, error)
}

type ProjectNativeWorkloadIdentityStatusWriter interface {
	SetProjectNativeWorkloadIdentityStatus(ctx context.Context, project string, status NativeWorkloadIdentityStatus) (Project, error)
}

type NativeWorkloadIdentityStatus struct {
	State              string                                   `json:"state"`
	Provider           string                                   `json:"provider,omitempty"`
	SubscriptionID     string                                   `json:"subscription_id,omitempty"`
	ResourceGroup      string                                   `json:"resource_group,omitempty"`
	Issuer             string                                   `json:"issuer,omitempty"`
	DesiredCount       int                                      `json:"desired_count"`
	ManagedCredentials []NativeWorkloadIdentityCredentialStatus `json:"managed_credentials"`
	Upserted           []NativeWorkloadIdentityCredentialStatus `json:"upserted,omitempty"`
	Deleted            []NativeWorkloadIdentityCredentialStatus `json:"deleted,omitempty"`
	LastReconciledAt   string                                   `json:"last_reconciled_at,omitempty"`
	LastError          *string                                  `json:"last_error,omitempty"`
}

type NativeWorkloadIdentityCredentialStatus struct {
	IdentityName   string   `json:"identity_name"`
	CredentialName string   `json:"credential_name"`
	Subject        string   `json:"subject"`
	Audiences      []string `json:"audiences,omitempty"`
}

type NativeWorkloadIdentityService struct {
	Client                  FederatedIdentityCredentialClient
	Issuer                  string
	ServiceAccountTokenPath string
	Now                     func() time.Time
}

type nativeWorkloadIdentityConfig struct {
	Enabled        bool
	Provider       string
	SubscriptionID string
	ResourceGroup  string
	Issuer         string
	SlotPrefix     string
	Count          int
	Credentials    []nativeWorkloadIdentityCredentialTemplate
}

type nativeWorkloadIdentityCredentialTemplate struct {
	IdentityName   string
	CredentialName string
	Subject        string
	Audiences      []string
}

func (s NativeWorkloadIdentityService) ReconcileNativeWorkloadIdentities(ctx context.Context, project Project) (NativeWorkloadIdentityStatus, error) {
	cfg, ok, err := nativeWorkloadIdentityConfigFromProject(project)
	if !ok {
		return NativeWorkloadIdentityStatus{}, nil
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	status := NativeWorkloadIdentityStatus{
		State:            NativeWorkloadIdentityStatusFailed,
		Provider:         cfg.Provider,
		SubscriptionID:   cfg.SubscriptionID,
		ResourceGroup:    cfg.ResourceGroup,
		DesiredCount:     cfg.Count,
		LastReconciledAt: now,
	}
	if err == nil {
		cfg.Issuer = firstNonEmpty(cfg.Issuer, strings.TrimSpace(s.Issuer), issuerFromServiceAccountToken(s.ServiceAccountTokenPath))
		status.Issuer = cfg.Issuer
		status.ManagedCredentials = credentialStatusList(desiredWorkloadIdentityCredentials(cfg))
	}
	if err != nil {
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	if cfg.Issuer == "" {
		err := errors.New("native_standby_workload_identity requires issuer or NATIVE_WORKLOAD_IDENTITY_ISSUER")
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	if s.Client == nil {
		err := errors.New("native workload identity client not configured")
		status.LastError = stringPtr(err.Error())
		return status, err
	}

	desired := desiredWorkloadIdentityCredentials(cfg)
	deleted, err := s.deleteRemovedManagedCredentials(ctx, cfg, desired)
	if err != nil {
		status.LastError = stringPtr(err.Error())
		return status, err
	}
	status.Deleted = credentialStatusList(deleted)

	for _, credential := range desired {
		if err := s.Client.UpsertFederatedIdentityCredential(ctx, credential); err != nil {
			err = fmt.Errorf("upsert federated identity credential %s/%s: %w", credential.IdentityName, credential.CredentialName, err)
			status.LastError = stringPtr(err.Error())
			return status, err
		}
		status.Upserted = append(status.Upserted, credentialStatus(credential))
	}

	status.State = NativeWorkloadIdentityStatusOK
	status.LastError = nil
	return status, nil
}

func (s NativeWorkloadIdentityService) deleteRemovedManagedCredentials(ctx context.Context, cfg nativeWorkloadIdentityConfig, desired []FederatedIdentityCredential) ([]FederatedIdentityCredential, error) {
	deleted := []FederatedIdentityCredential{}
	desiredSet := workloadIdentityCredentialSet(desired)
	seenIdentity := map[string]bool{}
	for _, template := range cfg.Credentials {
		if seenIdentity[template.IdentityName] {
			continue
		}
		seenIdentity[template.IdentityName] = true
		ref := FederatedIdentityCredentialRef{
			SubscriptionID: cfg.SubscriptionID,
			ResourceGroup:  cfg.ResourceGroup,
			IdentityName:   template.IdentityName,
		}
		current, err := s.Client.ListFederatedIdentityCredentials(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("list federated identity credentials for %s: %w", template.IdentityName, err)
		}
		for _, credential := range current {
			if desiredSet[workloadIdentityCredentialKey(credential)] {
				continue
			}
			if _, ok := managedWorkloadIdentityCredentialSlotName(credential, cfg); !ok {
				continue
			}
			if err := s.Client.DeleteFederatedIdentityCredential(ctx, credential.FederatedIdentityCredentialRef); err != nil {
				return nil, fmt.Errorf("delete federated identity credential %s/%s: %w", credential.IdentityName, credential.CredentialName, err)
			}
			deleted = append(deleted, credential)
		}
	}
	return deleted, nil
}

func (s NativeWorkloadIdentityService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func nativeWorkloadIdentityConfigFromProject(project Project) (nativeWorkloadIdentityConfig, bool, error) {
	cfgMap, ok := mapFromMap(project.Metadata, "native_standby_workload_identity")
	if !ok {
		cfgMap, ok = mapFromMap(project.Metadata, "nativeStandbyWorkloadIdentity")
	}
	if !ok || !boolFromMap(cfgMap, "enabled") {
		return nativeWorkloadIdentityConfig{}, false, nil
	}
	standby, standbyOK := mapFromMap(project.Metadata, "native_standby_dns")
	if !standbyOK {
		standby, standbyOK = mapFromMap(project.Metadata, "nativeStandbyDns")
	}
	cfg := nativeWorkloadIdentityConfig{
		Enabled:        true,
		Provider:       firstNonEmpty(stringMapValue(cfgMap, "provider"), "azure"),
		SubscriptionID: firstNonEmpty(stringMapValue(cfgMap, "subscription"), stringMapValue(cfgMap, "subscription_id"), stringMapValue(cfgMap, "subscriptionId")),
		ResourceGroup:  firstNonEmpty(stringMapValue(cfgMap, "resource_group"), stringMapValue(cfgMap, "resourceGroup")),
		Issuer:         firstNonEmpty(stringMapValue(cfgMap, "issuer"), stringMapValue(cfgMap, "issuer_url"), stringMapValue(cfgMap, "issuerUrl")),
		SlotPrefix:     firstNonEmpty(stringMapValue(standby, "slot_prefix"), stringMapValue(standby, "slotPrefix")),
		Count:          nonNegativeIntMapValue(cfgMap, "count"),
	}
	if cfg.Count == 0 {
		cfg.Count = nonNegativeIntMapValue(standby, "count")
	}
	switch cfg.Provider {
	case "", "azure":
		cfg.Provider = "azure"
	default:
		return cfg, true, fmt.Errorf("unsupported native workload identity provider %q", cfg.Provider)
	}
	if cfg.SubscriptionID == "" {
		return cfg, true, errors.New("native_standby_workload_identity.subscription is required")
	}
	if cfg.ResourceGroup == "" {
		return cfg, true, errors.New("native_standby_workload_identity.resource_group is required")
	}
	if !standbyOK {
		return cfg, true, errors.New("native_standby_dns metadata is required")
	}
	if cfg.SlotPrefix == "" {
		return cfg, true, errors.New("native_standby_dns.slot_prefix is required")
	}
	credentials := workloadIdentityCredentialTemplatesFromMap(cfgMap)
	if len(credentials) == 0 {
		return cfg, true, errors.New("native_standby_workload_identity.credentials is required")
	}
	cfg.Credentials = credentials
	return cfg, true, nil
}

func workloadIdentityCredentialTemplatesFromMap(values map[string]any) []nativeWorkloadIdentityCredentialTemplate {
	rows := anySlice(firstAny(values["credentials"], values["federated_credentials"], values["federatedCredentials"]))
	templates := make([]nativeWorkloadIdentityCredentialTemplate, 0, len(rows))
	for _, row := range rows {
		mapped := anyMap(row)
		template := nativeWorkloadIdentityCredentialTemplate{
			IdentityName:   firstNonEmpty(stringMapValue(mapped, "identity_name"), stringMapValue(mapped, "identityName")),
			CredentialName: firstNonEmpty(stringMapValue(mapped, "credential_name"), stringMapValue(mapped, "credentialName"), stringMapValue(mapped, "name")),
			Subject:        stringMapValue(mapped, "subject"),
			Audiences:      stringSliceFromMap(mapped, "audiences", "audience"),
		}
		if len(template.Audiences) == 0 {
			template.Audiences = []string{defaultWorkloadIdentityAudience}
		}
		if template.IdentityName == "" || template.CredentialName == "" || template.Subject == "" {
			continue
		}
		templates = append(templates, template)
	}
	return templates
}

func desiredWorkloadIdentityCredentials(cfg nativeWorkloadIdentityConfig) []FederatedIdentityCredential {
	credentials := make([]FederatedIdentityCredential, 0, cfg.Count*len(cfg.Credentials))
	for slotIndex := 1; slotIndex <= cfg.Count; slotIndex++ {
		slotName := fmt.Sprintf("%s-%d", cfg.SlotPrefix, slotIndex)
		substitutions := workloadIdentitySubstitutions(cfg, slotIndex, slotName)
		for _, template := range cfg.Credentials {
			credentials = append(credentials, FederatedIdentityCredential{
				FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
					SubscriptionID: cfg.SubscriptionID,
					ResourceGroup:  cfg.ResourceGroup,
					IdentityName:   template.IdentityName,
					CredentialName: formatSubstitutions(template.CredentialName, substitutions),
				},
				Issuer:    cfg.Issuer,
				Subject:   formatSubstitutions(template.Subject, substitutions),
				Audiences: append([]string{}, template.Audiences...),
			})
		}
	}
	sort.SliceStable(credentials, func(i, j int) bool {
		left, right := credentials[i], credentials[j]
		if left.IdentityName != right.IdentityName {
			return left.IdentityName < right.IdentityName
		}
		return left.CredentialName < right.CredentialName
	})
	return credentials
}

func workloadIdentityCredentialSet(credentials []FederatedIdentityCredential) map[string]bool {
	out := map[string]bool{}
	for _, credential := range credentials {
		out[workloadIdentityCredentialKey(credential)] = true
	}
	return out
}

func workloadIdentityCredentialKey(credential FederatedIdentityCredential) string {
	return credential.IdentityName + "\x00" + credential.CredentialName + "\x00" + credential.Subject
}

func workloadIdentitySubstitutions(cfg nativeWorkloadIdentityConfig, slotIndex int, slotName string) map[string]string {
	return map[string]string{
		"project":    cfg.SlotPrefix,
		"slot_index": strconv.Itoa(slotIndex),
		"slot_name":  slotName,
		"namespace":  slotName,
	}
}

func managedWorkloadIdentityCredentialIndex(credential FederatedIdentityCredential, cfg nativeWorkloadIdentityConfig) (int, bool) {
	slotName, ok := managedWorkloadIdentityCredentialSlotName(credential, cfg)
	if !ok {
		return 0, false
	}
	index := workloadIdentitySlotIndex(slotName)
	return index, index > 0
}

func managedWorkloadIdentityCredentialSlotName(credential FederatedIdentityCredential, cfg nativeWorkloadIdentityConfig) (string, bool) {
	for _, template := range cfg.Credentials {
		if credential.IdentityName != template.IdentityName {
			continue
		}
		for _, slotName := range workloadIdentitySlotNameCandidates(credential, template) {
			index := workloadIdentitySlotIndex(slotName)
			if index < 1 {
				continue
			}
			substitutions := workloadIdentitySubstitutions(cfg, index, slotName)
			if credential.CredentialName != formatSubstitutions(template.CredentialName, substitutions) {
				continue
			}
			if credential.Subject != formatSubstitutions(template.Subject, substitutions) {
				continue
			}
			return slotName, true
		}
	}
	return "", false
}

func workloadIdentitySlotNameCandidates(credential FederatedIdentityCredential, template nativeWorkloadIdentityCredentialTemplate) []string {
	seen := map[string]bool{}
	var candidates []string
	for _, candidate := range slotNameCandidatesFromTemplate(credential.CredentialName, template.CredentialName) {
		if !seen[candidate] {
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range slotNameCandidatesFromTemplate(credential.Subject, template.Subject) {
		if !seen[candidate] {
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	sort.Strings(candidates)
	return candidates
}

func slotNameCandidatesFromTemplate(value, template string) []string {
	if !strings.Contains(template, "{slot_name}") && !strings.Contains(template, "{namespace}") {
		return nil
	}
	pattern := regexp.QuoteMeta(template)
	slotPattern := `([a-z0-9](?:[a-z0-9-]*[a-z0-9])?-[1-9][0-9]*)`
	pattern = strings.ReplaceAll(pattern, `\{slot_name\}`, slotPattern)
	pattern = strings.ReplaceAll(pattern, `\{namespace\}`, slotPattern)
	pattern = strings.ReplaceAll(pattern, `\{slot_index\}`, `[1-9][0-9]*`)
	pattern = "^" + pattern + "$"
	matches := regexp.MustCompile(pattern).FindStringSubmatch(value)
	if len(matches) <= 1 {
		return nil
	}
	seen := map[string]bool{}
	var candidates []string
	for _, match := range matches[1:] {
		if match == "" || seen[match] {
			continue
		}
		seen[match] = true
		candidates = append(candidates, match)
	}
	return candidates
}

func workloadIdentitySlotIndex(slotName string) int {
	idx := strings.LastIndex(slotName, "-")
	if idx < 0 || idx == len(slotName)-1 {
		return 0
	}
	index, err := strconv.Atoi(slotName[idx+1:])
	if err != nil || index < 1 {
		return 0
	}
	return index
}

func credentialStatusList(credentials []FederatedIdentityCredential) []NativeWorkloadIdentityCredentialStatus {
	statuses := make([]NativeWorkloadIdentityCredentialStatus, 0, len(credentials))
	for _, credential := range credentials {
		statuses = append(statuses, credentialStatus(credential))
	}
	return statuses
}

func credentialStatus(credential FederatedIdentityCredential) NativeWorkloadIdentityCredentialStatus {
	return NativeWorkloadIdentityCredentialStatus{
		IdentityName:   credential.IdentityName,
		CredentialName: credential.CredentialName,
		Subject:        credential.Subject,
		Audiences:      append([]string{}, credential.Audiences...),
	}
}

func issuerFromServiceAccountToken(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(string(data)), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Issuer)
}
