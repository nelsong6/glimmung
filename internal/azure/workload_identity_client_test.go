package azure

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/nelsong6/glimmung/internal/server"
)

type fakeTokenCredential struct{}

func (fakeTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestWorkloadIdentityClientUpsertsFederatedCredential(t *testing.T) {
	var method, path string
	var body map[string]any
	client := &WorkloadIdentityClient{
		credential: fakeTokenCredential{},
		endpoint:   "https://management.example",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method = req.Method
			path = req.URL.Path
			if req.URL.Query().Get("api-version") != managedIdentityFICAPIVersion {
				t.Fatalf("api-version=%q", req.URL.Query().Get("api-version"))
			}
			if req.Header.Get("Authorization") != "Bearer token" {
				t.Fatalf("authorization=%q", req.Header.Get("Authorization"))
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		})},
	}

	err := client.UpsertFederatedIdentityCredential(context.Background(), server.FederatedIdentityCredential{
		FederatedIdentityCredentialRef: server.FederatedIdentityCredentialRef{
			SubscriptionID: "sub",
			ResourceGroup:  "infra",
			IdentityName:   "tank-session-identity",
			CredentialName: "tank-slot-1-session",
		},
		Issuer:    "https://issuer.example/",
		Subject:   "system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session",
		Audiences: []string{"api://AzureADTokenExchange"},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if method != http.MethodPut {
		t.Fatalf("method=%q", method)
	}
	wantPath := "/subscriptions/sub/resourceGroups/infra/providers/Microsoft.ManagedIdentity/userAssignedIdentities/tank-session-identity/federatedIdentityCredentials/tank-slot-1-session"
	if path != wantPath {
		t.Fatalf("path=%q want %q", path, wantPath)
	}
	props := body["properties"].(map[string]any)
	if props["subject"] != "system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session" {
		t.Fatalf("body=%#v", body)
	}
}

func TestWorkloadIdentityClientListsAndDeletesFederatedCredentials(t *testing.T) {
	var deleted string
	client := &WorkloadIdentityClient{
		credential: fakeTokenCredential{},
		endpoint:   "https://management.example",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.Method {
			case http.MethodGet:
				body := `{"value":[{"name":"tank-slot-1-session","properties":{"issuer":"https://issuer.example/","subject":"system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session","audiences":["api://AzureADTokenExchange"]}}]}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			case http.MethodDelete:
				deleted = req.URL.Path
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			default:
				t.Fatalf("unexpected method %s", req.Method)
			}
			return nil, nil
		})},
	}
	ref := server.FederatedIdentityCredentialRef{
		SubscriptionID: "sub",
		ResourceGroup:  "infra",
		IdentityName:   "tank-session-identity",
	}
	credentials, err := client.ListFederatedIdentityCredentials(context.Background(), ref)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(credentials) != 1 || credentials[0].CredentialName != "tank-slot-1-session" {
		t.Fatalf("credentials=%#v", credentials)
	}
	ref.CredentialName = "tank-slot-1-session"
	if err := client.DeleteFederatedIdentityCredential(context.Background(), ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.HasSuffix(deleted, "/federatedIdentityCredentials/tank-slot-1-session") {
		t.Fatalf("deleted path=%q", deleted)
	}
}
