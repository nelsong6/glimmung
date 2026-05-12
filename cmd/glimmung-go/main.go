package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
	entraredirects "github.com/nelsong6/glimmung/internal/entra"
	githubclient "github.com/nelsong6/glimmung/internal/github"
	"github.com/nelsong6/glimmung/internal/server"
	artifactstore "github.com/nelsong6/glimmung/internal/store/artifacts"
	cosmosstore "github.com/nelsong6/glimmung/internal/store/cosmos"
)

func main() {
	settings := server.SettingsFromEnv()
	store, err := cosmosstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("cosmos read store disabled: %v", err)
	}
	artifacts, err := artifactstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("artifact store disabled: %v", err)
	}
	authenticator := buildAuthenticator(settings)
	ghClient := buildGitHubClient(settings)
	authRedirectClient, err := entraredirects.NewRedirectClient()
	if err != nil {
		log.Printf("native auth redirect reconciler disabled: %v", err)
	}
	authRedirects := server.NativeAuthRedirectService{Client: authRedirectClient}
	var ghDispatch server.GHADispatchClient
	if d, ok := ghClient.(server.GHADispatchClient); ok {
		ghDispatch = d
	}
	if store != nil {
		go server.StartSignalDrainLoop(context.Background(), store, ghDispatch, 15*time.Second, log.Printf)
	}
	addr := ":" + settings.Port

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewWithRuntimeClients(settings, store, authenticator, ghClient, authRedirects, artifacts),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("starting glimmung-go on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}

type gitHubClientAdapter struct {
	client *githubclient.Client
}

func (a *gitHubClientAdapter) FetchWorkflowFile(ctx context.Context, repo, name, ref string) ([]byte, int, error) {
	path := ".glimmung/workflows/" + name + ".yaml"
	data, err := a.client.FetchFileContents(ctx, repo, path, ref)
	if errors.Is(err, githubclient.ErrNotFound) {
		return nil, 404, err
	}
	if err != nil {
		return nil, 502, err
	}
	return data, 200, nil
}

func (a *gitHubClientAdapter) DispatchWorkflow(ctx context.Context, repo, filename, ref string, inputs map[string]string) error {
	return a.client.DispatchWorkflow(ctx, repo, filename, ref, inputs)
}

func buildGitHubClient(settings server.Settings) server.WorkflowSyncClient {
	if settings.GitHubAppID == "" || settings.GitHubAppInstallationID == "" || settings.GitHubAppPrivateKey == "" {
		return nil
	}
	client, err := githubclient.New(settings.GitHubAppID, settings.GitHubAppInstallationID, settings.GitHubAppPrivateKey)
	if err != nil {
		log.Printf("GitHub App client disabled: %v", err)
		return nil
	}
	return &gitHubClientAdapter{client: client}
}

func buildAuthenticator(settings server.Settings) auth.CompositeAuthenticator {
	var entra *auth.EntraAuthenticator
	if settings.EntraClientID != "" || settings.EntraTestClientID != "" {
		authenticator, err := auth.NewEntraAuthenticator(auth.EntraConfig{
			Audiences:     []string{settings.EntraClientID, settings.EntraTestClientID},
			AllowedEmails: settings.AllowedEmails,
		})
		if err != nil {
			log.Printf("entra auth disabled: %v", err)
		} else {
			entra = authenticator
		}
	}

	var k8s *auth.K8sAuthenticator
	if settings.K8sSAAllowlist != "" {
		authenticator, err := auth.NewK8sAuthenticator(auth.K8sConfig{
			APIHost:      settings.K8sAPIHost,
			Allowlist:    settings.K8sSAAllowlist,
			OwnTokenPath: settings.K8sSATokenPath,
			CACertPath:   settings.K8sCACertPath,
		})
		if err != nil {
			log.Printf("k8s auth disabled: %v", err)
		} else {
			k8s = authenticator
		}
	}

	return auth.CompositeAuthenticator{Entra: entra, K8s: k8s}
}
