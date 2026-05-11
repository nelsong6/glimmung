package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/server"
	cosmosstore "github.com/nelsong6/glimmung/internal/store/cosmos"
)

func main() {
	settings := server.SettingsFromEnv()
	store, err := cosmosstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("cosmos read store disabled: %v", err)
	}
	authenticator := buildAuthenticator(settings)
	addr := ":" + settings.Port

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewWithDependencies(settings, store, authenticator),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("starting glimmung-go on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
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
