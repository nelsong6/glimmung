package entra

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
	defaultGraphEndpoint = "https://graph.microsoft.com/v1.0"
	graphScope           = "https://graph.microsoft.com/.default"
)

type RedirectClient struct {
	credential azcore.TokenCredential
	httpClient *http.Client
	endpoint   string
}

func NewRedirectClient() (*RedirectClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}
	return &RedirectClient{
		credential: cred,
		httpClient: http.DefaultClient,
		endpoint:   defaultGraphEndpoint,
	}, nil
}

func (c *RedirectClient) ReadApplicationRedirects(ctx context.Context, ref server.ApplicationRedirectRef) (server.ApplicationRedirectApp, error) {
	if strings.TrimSpace(ref.ObjectID) != "" {
		return c.readApplicationByObjectID(ctx, strings.TrimSpace(ref.ObjectID))
	}
	if strings.TrimSpace(ref.ClientID) == "" {
		return server.ApplicationRedirectApp{}, fmt.Errorf("application object id or client id required")
	}
	return c.readApplicationByClientID(ctx, strings.TrimSpace(ref.ClientID))
}

func (c *RedirectClient) UpdateApplicationRedirects(ctx context.Context, objectID string, redirectURIs []string) error {
	if strings.TrimSpace(objectID) == "" {
		return fmt.Errorf("application object id required")
	}
	body, err := json.Marshal(map[string]any{
		"spa": map[string]any{"redirectUris": redirectURIs},
	})
	if err != nil {
		return err
	}
	req, err := c.newGraphRequest(ctx, http.MethodPatch, "/applications/"+url.PathEscape(objectID), bytes.NewReader(body))
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
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Graph PATCH applications returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *RedirectClient) readApplicationByObjectID(ctx context.Context, objectID string) (server.ApplicationRedirectApp, error) {
	req, err := c.newGraphRequest(ctx, http.MethodGet, "/applications/"+url.PathEscape(objectID), nil)
	if err != nil {
		return server.ApplicationRedirectApp{}, err
	}
	q := req.URL.Query()
	q.Set("$select", "id,appId,spa")
	req.URL.RawQuery = q.Encode()
	var app graphApplication
	if err := c.doJSON(req, &app); err != nil {
		return server.ApplicationRedirectApp{}, err
	}
	return graphApplicationToServer(app), nil
}

func (c *RedirectClient) readApplicationByClientID(ctx context.Context, clientID string) (server.ApplicationRedirectApp, error) {
	req, err := c.newGraphRequest(ctx, http.MethodGet, "/applications", nil)
	if err != nil {
		return server.ApplicationRedirectApp{}, err
	}
	q := req.URL.Query()
	q.Set("$select", "id,appId,spa")
	q.Set("$filter", "appId eq '"+strings.ReplaceAll(clientID, "'", "''")+"'")
	req.URL.RawQuery = q.Encode()
	var list graphApplicationList
	if err := c.doJSON(req, &list); err != nil {
		return server.ApplicationRedirectApp{}, err
	}
	if len(list.Value) == 0 {
		return server.ApplicationRedirectApp{}, server.ErrNotFound
	}
	return graphApplicationToServer(list.Value[0]), nil
}

func (c *RedirectClient) newGraphRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{graphScope}})
	if err != nil {
		return nil, fmt.Errorf("get Graph token: %w", err)
	}
	endpoint := strings.TrimRight(firstNonEmpty(c.endpoint, defaultGraphEndpoint), "/")
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *RedirectClient) doJSON(req *http.Request, target any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return server.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Graph %s returned %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode Graph response: %w", err)
	}
	return nil
}

type graphApplicationList struct {
	Value []graphApplication `json:"value"`
}

type graphApplication struct {
	ID    string `json:"id"`
	AppID string `json:"appId"`
	SPA   struct {
		RedirectURIs []string `json:"redirectUris"`
	} `json:"spa"`
}

func graphApplicationToServer(app graphApplication) server.ApplicationRedirectApp {
	return server.ApplicationRedirectApp{
		ObjectID:     app.ID,
		ClientID:     app.AppID,
		RedirectURIs: app.SPA.RedirectURIs,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
