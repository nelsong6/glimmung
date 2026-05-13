package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeFederatedIdentityCredentialClient struct {
	current map[string][]FederatedIdentityCredential
	upserts []FederatedIdentityCredential
	deletes []FederatedIdentityCredentialRef
	err     error
}

func (c *fakeFederatedIdentityCredentialClient) UpsertFederatedIdentityCredential(_ context.Context, credential FederatedIdentityCredential) error {
	if c.err != nil {
		return c.err
	}
	c.upserts = append(c.upserts, credential)
	return nil
}

func (c *fakeFederatedIdentityCredentialClient) ListFederatedIdentityCredentials(_ context.Context, ref FederatedIdentityCredentialRef) ([]FederatedIdentityCredential, error) {
	if c.err != nil {
		return nil, c.err
	}
	return append([]FederatedIdentityCredential{}, c.current[ref.IdentityName]...), nil
}

func (c *fakeFederatedIdentityCredentialClient) DeleteFederatedIdentityCredential(_ context.Context, ref FederatedIdentityCredentialRef) error {
	if c.err != nil {
		return c.err
	}
	c.deletes = append(c.deletes, ref)
	return nil
}

func TestNativeWorkloadIdentitiesReconcilesManagedCredentials(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{
			"tank-session-identity": {{
				FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
					SubscriptionID: "sub",
					ResourceGroup:  "infra",
					IdentityName:   "tank-session-identity",
					CredentialName: "tank-slot-4-session",
				},
				Issuer:    "https://issuer.example/",
				Subject:   "system:serviceaccount:tank-slot-4-sessions:tank-slot-4-session",
				Audiences: []string{defaultWorkloadIdentityAudience},
			}, {
				FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
					SubscriptionID: "sub",
					ResourceGroup:  "infra",
					IdentityName:   "tank-session-identity",
					CredentialName: "tank-operator-2-session",
				},
				Issuer:    "https://issuer.example/",
				Subject:   "system:serviceaccount:tank-operator-2-sessions:tank-operator-2-session",
				Audiences: []string{defaultWorkloadIdentityAudience},
			}},
		},
	}
	service := NativeWorkloadIdentityService{
		Client: client,
		Issuer: "https://issuer.example/",
		Now:    func() time.Time { return time.Date(2026, 5, 13, 7, 0, 0, 0, time.UTC) },
	}
	project := Project{
		Name: "tank",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(3),
			},
			"native_standby_workload_identity": map[string]any{
				"enabled":        true,
				"subscription":   "sub",
				"resource_group": "infra",
				"count":          float64(3),
				"credentials": []any{
					map[string]any{
						"identity_name":   "tank-session-identity",
						"credential_name": "{slot_name}-session",
						"subject":         "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
					},
				},
			},
		},
	}

	status, err := service.ReconcileNativeWorkloadIdentities(context.Background(), project)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != NativeWorkloadIdentityStatusOK {
		t.Fatalf("status=%#v", status)
	}
	wantUpserts := []string{"tank-slot-1-session", "tank-slot-2-session", "tank-slot-3-session"}
	gotUpserts := make([]string, 0, len(client.upserts))
	for _, credential := range client.upserts {
		gotUpserts = append(gotUpserts, credential.CredentialName)
		if credential.Issuer != "https://issuer.example/" {
			t.Fatalf("issuer=%q", credential.Issuer)
		}
	}
	if !reflect.DeepEqual(gotUpserts, wantUpserts) {
		t.Fatalf("upserts=%#v want %#v", gotUpserts, wantUpserts)
	}
	gotDeletes := []string{}
	for _, deleted := range client.deletes {
		gotDeletes = append(gotDeletes, deleted.CredentialName)
	}
	if !reflect.DeepEqual(gotDeletes, []string{"tank-slot-4-session", "tank-operator-2-session"}) {
		t.Fatalf("deletes=%#v", client.deletes)
	}
	if len(status.ManagedCredentials) != 3 {
		t.Fatalf("managed=%#v", status.ManagedCredentials)
	}
	if len(status.Deleted) != 2 {
		t.Fatalf("deleted status=%#v", status.Deleted)
	}
}

func TestNativeWorkloadIdentitiesDoesNotDeleteManualLookalikes(t *testing.T) {
	cfg := nativeWorkloadIdentityConfig{
		SubscriptionID: "sub",
		ResourceGroup:  "infra",
		Issuer:         "https://issuer.example/",
		SlotPrefix:     "tank-slot",
		Count:          1,
		Credentials: []nativeWorkloadIdentityCredentialTemplate{{
			IdentityName:   "tank-session-identity",
			CredentialName: "{slot_name}-session",
			Subject:        "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
			Audiences:      []string{defaultWorkloadIdentityAudience},
		}},
	}
	cases := []FederatedIdentityCredential{
		{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{IdentityName: "tank-session-identity", CredentialName: "tank-slot-2-other"},
			Subject:                        "system:serviceaccount:tank-slot-2-sessions:tank-slot-2-session",
		},
		{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{IdentityName: "tank-session-identity", CredentialName: "tank-slot-2-session"},
			Subject:                        "system:serviceaccount:tank-slot-2-sessions:manual",
		},
		{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{IdentityName: "other-identity", CredentialName: "tank-slot-2-session"},
			Subject:                        "system:serviceaccount:tank-slot-2-sessions:tank-slot-2-session",
		},
	}
	for _, credential := range cases {
		if _, ok := managedWorkloadIdentityCredentialIndex(credential, cfg); ok {
			t.Fatalf("manual credential matched managed template: %#v", credential)
		}
	}
}

func TestNativeWorkloadIdentitiesSkipsUnchangedCredentials(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{
			"tank-session-identity": {
				{
					FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
						SubscriptionID: "sub",
						ResourceGroup:  "infra",
						IdentityName:   "tank-session-identity",
						CredentialName: "tank-slot-1-session",
					},
					Issuer:    "https://issuer.example/",
					Subject:   "system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session",
					Audiences: []string{defaultWorkloadIdentityAudience},
				},
				{
					FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
						SubscriptionID: "sub",
						ResourceGroup:  "infra",
						IdentityName:   "tank-session-identity",
						CredentialName: "tank-slot-2-session",
					},
					Issuer:    "https://old-issuer.example/",
					Subject:   "system:serviceaccount:tank-slot-2-sessions:tank-slot-2-session",
					Audiences: []string{defaultWorkloadIdentityAudience},
				},
			},
		},
	}
	service := NativeWorkloadIdentityService{
		Client: client,
		Issuer: "https://issuer.example/",
		Now:    func() time.Time { return time.Date(2026, 5, 13, 7, 0, 0, 0, time.UTC) },
	}
	project := Project{
		Name: "tank",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(2),
			},
			"native_standby_workload_identity": map[string]any{
				"enabled":        true,
				"subscription":   "sub",
				"resource_group": "infra",
				"count":          float64(2),
				"credentials": []any{
					map[string]any{
						"identity_name":   "tank-session-identity",
						"credential_name": "{slot_name}-session",
						"subject":         "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
					},
				},
			},
		},
	}

	status, err := service.ReconcileNativeWorkloadIdentities(context.Background(), project)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != NativeWorkloadIdentityStatusOK {
		t.Fatalf("status=%#v", status)
	}
	if len(client.upserts) != 1 || client.upserts[0].CredentialName != "tank-slot-2-session" {
		t.Fatalf("upserts=%#v", client.upserts)
	}
	if len(status.Upserted) != 1 || status.Upserted[0].CredentialName != "tank-slot-2-session" {
		t.Fatalf("upserted status=%#v", status.Upserted)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("deletes=%#v", client.deletes)
	}
}

func TestNativeWorkloadIdentitiesSkippedWhenDisabled(t *testing.T) {
	service := NativeWorkloadIdentityService{Client: &fakeFederatedIdentityCredentialClient{}}
	status, err := service.ReconcileNativeWorkloadIdentities(context.Background(), Project{Metadata: map[string]any{}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != "" {
		t.Fatalf("status=%#v", status)
	}
}

func TestNativeWorkloadIdentitiesReportsConfigErrors(t *testing.T) {
	service := NativeWorkloadIdentityService{Client: &fakeFederatedIdentityCredentialClient{}}
	status, err := service.ReconcileNativeWorkloadIdentities(context.Background(), Project{
		Name: "tank",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{"slot_prefix": "tank-slot", "count": float64(1)},
			"native_standby_workload_identity": map[string]any{
				"enabled": true,
			},
		},
	})
	if err == nil {
		t.Fatal("expected config error")
	}
	if status.State != NativeWorkloadIdentityStatusFailed || status.LastError == nil {
		t.Fatalf("status=%#v", status)
	}
}

func TestNativeWorkloadIdentitiesReportsClientErrors(t *testing.T) {
	service := NativeWorkloadIdentityService{
		Client: &fakeFederatedIdentityCredentialClient{err: errors.New("boom")},
		Issuer: "https://issuer.example/",
	}
	status, err := service.ReconcileNativeWorkloadIdentities(context.Background(), Project{
		Name: "tank",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{"slot_prefix": "tank-slot", "count": float64(1)},
			"native_standby_workload_identity": map[string]any{
				"enabled":        true,
				"subscription":   "sub",
				"resource_group": "infra",
				"credentials": []any{map[string]any{
					"identity_name":   "tank-session-identity",
					"credential_name": "{slot_name}-session",
					"subject":         "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
				}},
			},
		},
	})
	if err == nil {
		t.Fatal("expected client error")
	}
	if status.State != NativeWorkloadIdentityStatusFailed || status.LastError == nil {
		t.Fatalf("status=%#v", status)
	}
}
