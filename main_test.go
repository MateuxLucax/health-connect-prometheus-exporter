package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeSamples(t *testing.T) {
	cases := []struct {
		name, body string
		ok         bool
	}{
		{"valid", `{"samples":[{"metric":"health_heart_rate_bpm","labels":{"source":"com.sec.android.app.shealth"},"value":72,"timestamp_ms":1792839600000}]}`, true},
		{"empty list", `{"samples":[]}`, true},
		{"colon in metric", `{"samples":[{"metric":"a:b","value":1,"timestamp_ms":1}]}`, true},
		{"not json", `nope`, false},
		{"bad metric", `{"samples":[{"metric":"1abc","value":1,"timestamp_ms":1}]}`, false},
		{"empty metric", `{"samples":[{"metric":"","value":1,"timestamp_ms":1}]}`, false},
		{"bad label", `{"samples":[{"metric":"m","labels":{"a-b":"x"},"value":1,"timestamp_ms":1}]}`, false},
		{"colon in label", `{"samples":[{"metric":"m","labels":{"a:b":"x"},"value":1,"timestamp_ms":1}]}`, false},
		{"reserved label", `{"samples":[{"metric":"m","labels":{"__name__":"x"},"value":1,"timestamp_ms":1}]}`, false},
		{"missing value", `{"samples":[{"metric":"m","timestamp_ms":1}]}`, false},
		{"zero timestamp", `{"samples":[{"metric":"m","value":1,"timestamp_ms":0}]}`, false},
		{"negative timestamp", `{"samples":[{"metric":"m","value":1,"timestamp_ms":-5}]}`, false},
	}
	for _, c := range cases {
		_, err := decodeSamples(strings.NewReader(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestHandler(t *testing.T) {
	const valid = `{"samples":[{"metric":"m","value":1,"timestamp_ms":1000}]}`
	cases := []struct {
		name       string
		auth, body string
		upstream   int // 0 = receiver unreachable
		want       int
	}{
		{"ok", "Bearer secret", valid, 204, 204},
		{"empty", "Bearer secret", `{"samples":[]}`, 204, 204},
		{"no token", "", valid, 204, 401},
		{"wrong token", "Bearer nope", valid, 204, 401},
		{"bad body", "Bearer secret", `{"samples":[{"metric":"m"}]}`, 204, 400},
		{"rejected sample", "Bearer secret", valid, 400, 204},
		{"receiver error", "Bearer secret", valid, 500, 502},
		{"receiver throttled", "Bearer secret", valid, 429, 502},
		{"receiver down", "Bearer secret", valid, 0, 502},
	}
	for _, c := range cases {
		rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.upstream)
		}))
		if c.upstream == 0 {
			rx.Close()
		}
		h := newHandler("secret", &remoteWriter{url: rx.URL, client: http.DefaultClient})
		req := httptest.NewRequest("POST", "/api/v1/samples", strings.NewReader(c.body))
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
		rx.Close()
	}
}

func TestHealthz(t *testing.T) {
	h := newHandler("secret", &remoteWriter{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
}
