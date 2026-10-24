// Command health-connect-prometheus-exporter receives health samples from the
// health-connect-bridge Android app and forwards them to Prometheus using the
// remote write 1.0 protocol, keeping each sample's original timestamp.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const maxBodyBytes = 32 << 20

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

type sample struct {
	Metric      string            `json:"metric"`
	Labels      map[string]string `json:"labels"`
	Value       *float64          `json:"value"`
	TimestampMs int64             `json:"timestamp_ms"`
}

type request struct {
	Samples []sample `json:"samples"`
}

func main() {
	token := os.Getenv("AUTH_TOKEN")
	if token == "" {
		log.Fatal("AUTH_TOKEN is required")
	}
	writeURL := os.Getenv("REMOTE_WRITE_URL")
	if writeURL == "" {
		log.Fatal("REMOTE_WRITE_URL is required")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	w := &remoteWriter{url: writeURL, client: &http.Client{Timeout: 30 * time.Second}}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(token, w),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s, writing to %s", addr, writeURL)
	log.Fatal(srv.ListenAndServe())
}

func newHandler(token string, w *remoteWriter) http.Handler {
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
		err = w.write(r.Context(), groupSeries(samples))
		var rejected *rejectedError
		switch {
		case errors.As(err, &rejected):
			// Retrying will never help (e.g. sample too old): drop and acknowledge.
			log.Printf("dropped %d of %d samples, last reason: %v", rejected.dropped, len(samples), err)
		case err != nil:
			log.Printf("remote write failed: %v", err)
			http.Error(rw, "remote write failed", http.StatusBadGateway)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func decodeSamples(body io.Reader) ([]sample, error) {
	var req request
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %v", err)
	}
	for i, s := range req.Samples {
		if !metricNameRE.MatchString(s.Metric) {
			return nil, fmt.Errorf("sample %d: invalid metric name %q", i, s.Metric)
		}
		for name := range s.Labels {
			if !labelNameRE.MatchString(name) || strings.HasPrefix(name, "__") {
				return nil, fmt.Errorf("sample %d: invalid label name %q", i, name)
			}
		}
		if s.Value == nil {
			return nil, fmt.Errorf("sample %d: missing value", i)
		}
		if s.TimestampMs <= 0 {
			return nil, fmt.Errorf("sample %d: timestamp_ms must be > 0", i)
		}
	}
	return req.Samples, nil
}
