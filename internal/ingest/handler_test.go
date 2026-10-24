package ingest

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MateuxLucax/health-connect-prometheus-exporter/internal/remotewrite"
)

// testClient is used for every HTTP request a test makes, so a hung server
// fails the test instead of blocking it forever.
var testClient = &http.Client{Timeout: time.Minute}

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
		{"receiver disabled", "Bearer secret", valid, 404, 502},
		{"proxy auth", "Bearer secret", valid, 401, 502},
		{"proxy forbidden", "Bearer secret", valid, 403, 502},
		{"wrong method", "Bearer secret", valid, 405, 502},
		{"receiver size limit", "Bearer secret", valid, 413, 502},
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
		h := NewHandler("secret", remotewrite.NewClient(rx.URL, testClient))
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
	h := NewHandler("secret", remotewrite.NewClient("", testClient))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
}

func TestDropFuture(t *testing.T) {
	const maxMs = 1000
	in := []remotewrite.Sample{
		{Metric: "m", Value: 1, TimestampMs: 1},
		{Metric: "m", Value: 2, TimestampMs: maxMs},
		{Metric: "m", Value: 3, TimestampMs: maxMs + 1},
		{Metric: "m", Value: 4, TimestampMs: 500},
	}
	kept, dropped := dropFuture(in, maxMs)
	want := []remotewrite.Sample{
		{Metric: "m", Value: 1, TimestampMs: 1},
		{Metric: "m", Value: 2, TimestampMs: maxMs},
		{Metric: "m", Value: 4, TimestampMs: 500},
	}
	if dropped != 1 || !reflect.DeepEqual(kept, want) {
		t.Errorf("got %v, %d dropped; want %v, 1 dropped", kept, dropped, want)
	}

	kept, dropped = dropFuture([]remotewrite.Sample{{Metric: "m", TimestampMs: maxMs + 1}}, maxMs)
	if dropped != 1 || len(kept) != 0 {
		t.Errorf("all future: got %v, %d dropped; want none kept, 1 dropped", kept, dropped)
	}
}
