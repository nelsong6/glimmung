package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nelsong6/glimmung/internal/server"
)

func main() {
	settings := server.SettingsFromEnv()
	addr := ":" + settings.Port

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(settings),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("starting glimmung-go on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}
