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
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	maxBodyBytes     = 8 << 20
	maxLabels        = 16
	maxLabelValueLen = 1024
	minTokenLen      = 32
	// Samples further in the future than this are dropped: they would move
	// Prometheus' head time forward and make normal samples "too old".
	maxFuture = time.Hour
)

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

	w := &remoteWriter{url: writeURL, client: &http.Client{Timeout: 30 * time.Second}}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(token, w),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: forwarding a large request upstream can be slow.
	}
	log.Printf("listening on %s, writing to %s", addr, parsedURL.Redacted())
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
		samples, future := dropFuture(samples, time.Now().Add(maxFuture).UnixMilli())
		if future > 0 {
			log.Printf("dropped %d samples more than %v in the future", future, maxFuture)
		}
		err = w.write(r.Context(), groupSeries(samples))
		var rejected *rejectedError
		switch {
		case errors.As(err, &rejected):
			// Prometheus answered 400 (e.g. sample too old): retrying will
			// never help, so drop those samples and acknowledge the rest.
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
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: unexpected data after the top-level value")
	}
	for i, s := range req.Samples {
		if !metricNameRE.MatchString(s.Metric) {
			return nil, fmt.Errorf("sample %d: invalid metric name %q", i, s.Metric)
		}
		if len(s.Labels) > maxLabels {
			return nil, fmt.Errorf("sample %d: more than %d labels", i, maxLabels)
		}
		for name, value := range s.Labels {
			if !labelNameRE.MatchString(name) || strings.HasPrefix(name, "__") {
				return nil, fmt.Errorf("sample %d: invalid label name %q", i, name)
			}
			if len(value) > maxLabelValueLen {
				return nil, fmt.Errorf("sample %d: label %q value longer than %d bytes", i, name, maxLabelValueLen)
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
