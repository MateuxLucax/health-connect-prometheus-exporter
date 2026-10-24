// Package ingest is the HTTP API the health-connect-bridge app talks to: it
// authenticates requests, validates the JSON samples and forwards them with a
// remote write client.
package ingest

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/MateuxLucax/health-connect-prometheus-exporter/internal/remotewrite"
)

// Samples further in the future than this are dropped: they would move
// Prometheus' head time forward and make normal samples "too old".
const maxFuture = time.Hour

// NewHandler returns the exporter's HTTP handler: GET /healthz and
// POST /api/v1/samples, which requires "Authorization: Bearer <token>" and
// writes the samples with w.
func NewHandler(token string, w *remotewrite.Client) http.Handler {
	want := []byte("Bearer " + token)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, "ok")
	})
	mux.HandleFunc("POST /api/v1/samples", func(rw http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		samples, err := decodeSamples(http.MaxBytesReader(rw, r.Body, maxBodyBytes))
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		samples, future := dropFuture(samples, time.Now().Add(maxFuture).UnixMilli())
		if future > 0 {
			log.Printf("dropped %d samples more than %v in the future", future, maxFuture)
		}
		out := make([]remotewrite.Sample, len(samples))
		for i, s := range samples {
			out[i] = s.toRemoteWrite()
		}
		err = w.Write(r.Context(), out)
		var rejected *remotewrite.RejectedError
		switch {
		case errors.As(err, &rejected):
			// Prometheus answered 400 (e.g. sample too old): retrying will
			// never help, so drop those samples and acknowledge the rest.
			log.Printf("dropped %d of %d samples, last reason: %v", rejected.Dropped, len(samples), err)
		case err != nil:
			log.Printf("remote write failed: %v", err)
			http.Error(rw, "remote write failed", http.StatusBadGateway)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// dropFuture removes samples with a timestamp after maxMs and returns the
// remaining samples and the number removed.
func dropFuture(samples []sample, maxMs int64) ([]sample, int) {
	kept := samples[:0]
	for _, s := range samples {
		if s.TimestampMs <= maxMs {
			kept = append(kept, s)
		}
	}
	return kept, len(samples) - len(kept)
}
