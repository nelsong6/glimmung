package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
	azureclient "github.com/nelsong6/glimmung/internal/azure"
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
	workloadIdentityClient, err := azureclient.NewWorkloadIdentityClient()
	if err != nil {
		log.Printf("native workload identity reconciler disabled: %v", err)
	}
	workloadIdentities := server.NativeWorkloadIdentityService{
		Client:                  workloadIdentityClient,
		Issuer:                  settings.NativeWorkloadIdentityIssuer,
		ServiceAccountTokenPath: settings.K8sSATokenPath,
	}
	// Glimmung-owned auth.romaine.life slot origin reconciler. Uses a
	// projected SA token mounted with audience = auth.romaine.life so it
	// cannot be replayed against other JWT validators. See glimmung#142.
	managedOrigins := server.ManagedOriginService{
		AuthBaseURL:             settings.AuthRomaineLifeBaseURL,
		ServiceAccountTokenPath: settings.AuthRomaineLifeTokenPath,
	}
	nativeLauncher := server.NewKubernetesNativeLauncher(settings)
	if store != nil {
		go server.StartSignalDrainLoop(context.Background(), store, nativeLauncher, 15*time.Second, log.Printf)
	}
	if store != nil {
		if nativeMinter, ok := ghClient.(server.NativeGitHubTokenMinter); ok {
			// One-shot recovery sweep at startup: re-arm per-lease TTL
			// timers, resume in-flight warming/activating/cleaning work, and
			// warm any slots that should exist by count but have no record
			// yet. After this returns, the test-slot lifecycle is purely
			// event-driven — HTTP handlers and per-lease AfterFunc timers,
			// no polling loop.
			go server.RecoverInFlightTestSlots(context.Background(), store, nativeLauncher, nativeMinter, log.Printf)
		}
	}
	addr := ":" + settings.Port

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewWithReconcilers(settings, store, authenticator, ghClient, workloadIdentities, managedOrigins, nativeLauncher, artifacts),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown: SIGTERM (from k8s eviction or node drain) triggers
	// HTTP draining followed by a bounded wait for in-flight test-slot
	// goroutines (warmup, activation, cleanup) to finish their Helm
	// operations. Without this, a pod evicted mid-Helm-install leaves a
	// partial release that the next pod's recovery sweep has to clean up,
	// and inbound HTTP requests get dropped instead of completing.
	//
	// The wait budget is sized to fit inside the Pod's
	// terminationGracePeriodSeconds (300s in the chart) with margin for
	// the HTTP drain and final cleanup. A Helm operation longer than this
	// will be cut off; the next pod's recovery sweep handles it.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-signalCtx.Done()
		log.Printf("shutdown signal received; draining HTTP server")
		httpCtx, httpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := srv.Shutdown(httpCtx); err != nil {
			log.Printf("http server shutdown error: %v", err)
		}
		httpCancel()

		log.Printf("waiting for in-flight test-slot goroutines to finish")
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		if err := server.WaitForInflightTestSlots(waitCtx); err != nil {
			log.Printf("in-flight test-slot wait exceeded budget: %v (orphans will be picked up by next pod's recovery sweep)", err)
		} else {
			log.Printf("in-flight test-slot goroutines drained")
		}
		waitCancel()
	}()

	log.Printf("starting glimmung-go on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
	// Wait for the shutdown goroutine to finish its drain + wait before exiting.
	<-shutdownDone
	log.Printf("glimmung-go shutdown complete")
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

func (a *gitHubClientAdapter) InstallationToken(ctx context.Context) (string, error) {
	return a.client.InstallationToken(ctx)
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
	// Microsoft sign-in is delegated to auth.romaine.life. The CookieDelegate
	// forwards the inbound .romaine.life session cookie to the auth service's
	// get-session endpoint per request (cached 60s) and gates on the role
	// claim (admin / user); no per-app config needed.
	cookieDelegate := auth.NewCookieDelegate()

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

	return auth.CompositeAuthenticator{Cookie: cookieDelegate, K8s: k8s}
}
