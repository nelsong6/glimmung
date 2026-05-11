package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nelsong6/glimmung/internal/server"
	cosmosstore "github.com/nelsong6/glimmung/internal/store/cosmos"
)

func main() {
	settings := server.SettingsFromEnv()
	store, err := cosmosstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("cosmos read store disabled: %v", err)
	}
	addr := ":" + settings.Port

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewWithStore(settings, store),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("starting glimmung-go on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}
