// Command health-connect-prometheus-exporter receives health samples from the
// health-connect-bridge Android app and forwards them to Prometheus using the
// remote write 1.0 protocol, keeping each sample's original timestamp.
//
// This file reads the configuration from the environment and wires the
// packages together: internal/ingest (HTTP API) and internal/remotewrite
// (Prometheus client).
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/MateuxLucax/health-connect-prometheus-exporter/internal/ingest"
	"github.com/MateuxLucax/health-connect-prometheus-exporter/internal/remotewrite"
)

const minTokenLen = 32

func main() {
	token := os.Getenv("AUTH_TOKEN")
	if len(token) < minTokenLen {
		log.Fatalf("AUTH_TOKEN must be at least %d characters (generate one with: openssl rand -hex 32)", minTokenLen)
	}
	writeURL := os.Getenv("REMOTE_WRITE_URL")
	if writeURL == "" {
		log.Fatal("REMOTE_WRITE_URL is required")
	}
	parsedURL, err := url.Parse(writeURL)
	if err != nil {
		log.Fatalf("REMOTE_WRITE_URL: %v", err)
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	w := remotewrite.NewClient(writeURL, &http.Client{Timeout: 30 * time.Second})
	srv := &http.Server{
		Addr:              addr,
		Handler:           ingest.NewHandler(token, w),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: forwarding a large request upstream can be slow.
	}
	log.Printf("listening on %s, writing to %s", addr, parsedURL.Redacted())
	log.Fatal(srv.ListenAndServe())
}
