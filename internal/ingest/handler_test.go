package ingest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestHandlerDropsFutureSamples(t *testing.T) {
	var got []specSeries
	calls := 0
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw, _ := specSnappyDecode(body)
		ss, _ := specDecodeWriteRequest(raw)
		got = append(got, ss...)
		calls++
		w.WriteHeader(204)
	}))
	defer rx.Close()
	h := NewHandler("secret", remotewrite.NewClient(rx.URL, testClient))
	post := func(body string) int {
		req := httptest.NewRequest("POST", "/api/v1/samples", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	now := time.Now().UnixMilli()
	okTs := now + int64(59*time.Minute/time.Millisecond)
	body := fmt.Sprintf(`{"samples":[
		{"metric":"m","value":1,"timestamp_ms":%d},
		{"metric":"m","value":2,"timestamp_ms":%d},
		{"metric":"m","value":3,"timestamp_ms":%d}]}`,
		now, okTs, now+int64(2*time.Hour/time.Millisecond))
	if code := post(body); code != 204 {
		t.Fatalf("status %d, want 204", code)
	}
	want := []specSample{{1, now}, {2, okTs}}
	if len(got) != 1 || fmt.Sprint(got[0].Samples) != fmt.Sprint(want) {
		t.Errorf("forwarded %+v, want samples %v", got, want)
	}

	// Only future samples: acknowledged without calling upstream.
	calls = 0
	if code := post(fmt.Sprintf(`{"samples":[{"metric":"m","value":1,"timestamp_ms":%d}]}`, now+int64(48*time.Hour/time.Millisecond))); code != 204 {
		t.Errorf("all future: status %d, want 204", code)
	}
	if calls != 0 {
		t.Errorf("all future: %d upstream calls, want 0", calls)
	}
}
