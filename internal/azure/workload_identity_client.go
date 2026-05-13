package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/nelsong6/glimmung/internal/server"
)

const (
	defaultARMEndpoint                 = "https://management.azure.com"
	armScope                           = "https://management.azure.com/.default"
	managedIdentityFICAPIVersion       = "2024-11-30"
	managedIdentityCredentialBatchSize = 4096
)

type WorkloadIdentityClient struct {
	credential azcore.TokenCredential
	httpClient *http.Client
	endpoint   string
}

func NewWorkloadIdentityClient() (*WorkloadIdentityClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}
	return &WorkloadIdentityClient{
		credential: cred,
		httpClient: http.DefaultClient,
		endpoint:   defaultARMEndpoint,
	}, nil
}

func (c *WorkloadIdentityClient) UpsertFederatedIdentityCredential(ctx context.Context, credential server.FederatedIdentityCredential) error {
	if err := validateCredentialRef(credential.FederatedIdentityCredentialRef); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{
			"issuer":    credential.Issuer,
			"subject":   credential.Subject,
			"audiences": credential.Audiences,
		},
	})
	if err != nil {
		return err
	}
	req, err := c.newARMRequest(ctx, http.MethodPut, federatedCredentialPath(credential.FederatedIdentityCredentialRef), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, managedIdentityCredentialBatchSize))
		return fmt.Errorf("ARM PUT federatedIdentityCredentials returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *WorkloadIdentityClient) ListFederatedIdentityCredentials(ctx context.Context, ref server.FederatedIdentityCredentialRef) ([]server.FederatedIdentityCredential, error) {
	if strings.TrimSpace(ref.SubscriptionID) == "" || strings.TrimSpace(ref.ResourceGroup) == "" || strings.TrimSpace(ref.IdentityName) == "" {
		return nil, fmt.Errorf("subscription, resource group, and identity name are required")
	}
	path := federatedCredentialCollectionPath(ref)
	var credentials []server.FederatedIdentityCredential
	for path != "" {
		req, err := c.newARMRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page federatedCredentialListResponse
		if err := c.doJSON(req, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Value {
			credentials = append(credentials, server.FederatedIdentityCredential{
				FederatedIdentityCredentialRef: server.FederatedIdentityCredentialRef{
					SubscriptionID: ref.SubscriptionID,
					ResourceGroup:  ref.ResourceGroup,
					IdentityName:   ref.IdentityName,
					CredentialName: item.Name,
				},
				Issuer:    item.Properties.Issuer,
				Subject:   item.Properties.Subject,
				Audiences: append([]string{}, item.Properties.Audiences...),
			})
		}
		path = strings.TrimSpace(page.NextLink)
	}
	return credentials, nil
}

func (c *WorkloadIdentityClient) DeleteFederatedIdentityCredential(ctx context.Context, ref server.FederatedIdentityCredentialRef) error {
	if err := validateCredentialRef(ref); err != nil {
		return err
	}
	req, err := c.newARMRequest(ctx, http.MethodDelete, federatedCredentialPath(ref), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, managedIdentityCredentialBatchSize))
		return fmt.Errorf("ARM DELETE federatedIdentityCredentials returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *WorkloadIdentityClient) newARMRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armScope}})
	if err != nil {
		return nil, fmt.Errorf("get ARM token: %w", err)
	}
	rawURL := path
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		rawURL = strings.TrimRight(firstNonEmpty(c.endpoint, defaultARMEndpoint), "/") + rawURL
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	if q.Get("api-version") == "" {
		q.Set("api-version", managedIdentityFICAPIVersion)
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *WorkloadIdentityClient) doJSON(req *http.Request, target any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return server.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, managedIdentityCredentialBatchSize))
		return fmt.Errorf("ARM %s returned %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode ARM response: %w", err)
	}
	return nil
}

func validateCredentialRef(ref server.FederatedIdentityCredentialRef) error {
	if strings.TrimSpace(ref.SubscriptionID) == "" {
		return fmt.Errorf("subscription id required")
	}
	if strings.TrimSpace(ref.ResourceGroup) == "" {
		return fmt.Errorf("resource group required")
	}
	if strings.TrimSpace(ref.IdentityName) == "" {
		return fmt.Errorf("identity name required")
	}
	if strings.TrimSpace(ref.CredentialName) == "" {
		return fmt.Errorf("credential name required")
	}
	return nil
}

func federatedCredentialCollectionPath(ref server.FederatedIdentityCredentialRef) string {
	return "/subscriptions/" + url.PathEscape(strings.TrimSpace(ref.SubscriptionID)) +
		"/resourceGroups/" + url.PathEscape(strings.TrimSpace(ref.ResourceGroup)) +
		"/providers/Microsoft.ManagedIdentity/userAssignedIdentities/" + url.PathEscape(strings.TrimSpace(ref.IdentityName)) +
		"/federatedIdentityCredentials"
}

func federatedCredentialPath(ref server.FederatedIdentityCredentialRef) string {
	return federatedCredentialCollectionPath(ref) + "/" + url.PathEscape(strings.TrimSpace(ref.CredentialName))
}

type federatedCredentialListResponse struct {
	Value    []federatedCredentialResource `json:"value"`
	NextLink string                        `json:"nextLink"`
}

type federatedCredentialResource struct {
	Name       string `json:"name"`
	Properties struct {
		Issuer    string   `json:"issuer"`
		Subject   string   `json:"subject"`
		Audiences []string `json:"audiences"`
	} `json:"properties"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
