package remotewrite

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// testClient is used for every HTTP request a test makes, so a hung server
// fails the test instead of blocking it forever.
var testClient = &http.Client{Timeout: time.Minute}

func TestSendHeaders(t *testing.T) {
	var hdr http.Header
	var body []byte
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	}))
	defer rx.Close()
	s := []series{{labels: []label{{"__name__", "m"}}, points: []point{{1, 2}}}}
	if err := NewClient(rx.URL, testClient).write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"Content-Encoding":                  "snappy",
		"Content-Type":                      "application/x-protobuf",
		"X-Prometheus-Remote-Write-Version": "0.1.0",
	} {
		if hdr.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, hdr.Get(k), v)
		}
	}
	if want := snappyEncode(encodeWriteRequest(s)); !bytes.Equal(body, want) {
		t.Errorf("body % x, want % x", body, want)
	}
}

func TestWriteIsolatesRejectedSamples(t *testing.T) {
	// Like Prometheus: reject the whole request if it contains a bad sample.
	var stored []int64
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ok []int64
		for ts := int64(1); ts <= 5; ts++ {
			// Each sample ends with value 0.0 (zero bytes), then tag 0x10 and ts.
			if bytes.Contains(body, []byte{0x00, 0x10, byte(ts)}) {
				if ts == 2 || ts == 4 {
					w.WriteHeader(400)
					return
				}
				ok = append(ok, ts)
			}
		}
		stored = append(stored, ok...)
		w.WriteHeader(204)
	}))
	defer rx.Close()
	s := []series{{labels: []label{{"__name__", "m"}}, points: []point{{0, 1}, {0, 2}, {0, 3}, {0, 4}, {0, 5}}}}
	err := NewClient(rx.URL, testClient).write(t.Context(), s)
	rej, ok := err.(*RejectedError)
	if !ok || rej.Dropped != 2 {
		t.Fatalf("err = %v, want 2 dropped", err)
	}
	if want := []int64{1, 3, 5}; !reflect.DeepEqual(stored, want) {
		t.Errorf("stored %v, want %v", stored, want)
	}
}

// tooOldProm is a fake Prometheus that, like Prometheus 3, rejects a whole
// request with 400 "too old sample" if any sample is older than cutoff.
type tooOldProm struct {
	cutoff int64
	mu     sync.Mutex
	calls  int
	stored map[int64]float64
}

func (p *tooOldProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	raw, err := specSnappyDecode(body)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	ss, _ := specDecodeWriteRequest(raw)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	for _, s := range ss {
		for _, sm := range s.Samples {
			if sm.Ts < p.cutoff {
				http.Error(w, "too old sample", http.StatusBadRequest)
				return
			}
		}
	}
	for _, s := range ss {
		for _, sm := range s.Samples {
			p.stored[sm.Ts] = sm.Value
		}
	}
	w.WriteHeader(204)
}

func TestIsTooOld(t *testing.T) {
	for msg, want := range map[string]bool{
		"remote write rejected (400): too old sample":                        true,
		"remote write rejected (400): out of bounds":                         true,
		"remote write rejected (400): duplicate sample for timestamp":        false,
		"remote write rejected (400): out of order sample":                   false,
		"remote write rejected (400): label name \"__name__\" is not unique": false,
	} {
		if got := isTooOld(msg); got != want {
			t.Errorf("isTooOld(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestWriteAllTooOldIsLogarithmic(t *testing.T) {
	const n = 5000
	p := &tooOldProm{cutoff: math.MaxInt64, stored: map[int64]float64{}}
	rx := httptest.NewServer(p)
	defer rx.Close()
	pts := make([]point, n)
	for i := range pts {
		pts[i] = point{float64(i), int64(i + 1)}
	}
	s := []series{{labels: []label{{"__name__", "m"}}, points: pts}}
	err := NewClient(rx.URL, testClient).write(t.Context(), s)
	rej, ok := err.(*RejectedError)
	if !ok || rej.Dropped != n {
		t.Fatalf("err = %v, want %d dropped", err, n)
	}
	// One batch request plus ceil(log2(5000)) = 13 single-sample probes.
	if p.calls > 1+14 {
		t.Errorf("%d upstream calls, want O(log n) (<= 15)", p.calls)
	}
	if len(p.stored) != 0 {
		t.Errorf("stored %d samples, want 0", len(p.stored))
	}
}

func TestWriteMixedTooOldKeepsValidSamples(t *testing.T) {
	const n, cutoff = 5000, 3000
	p := &tooOldProm{cutoff: cutoff, stored: map[int64]float64{}}
	rx := httptest.NewServer(p)
	defer rx.Close()
	// Several series with timestamps 1..n spread across them in random order.
	rnd := rand.New(rand.NewSource(1))
	var s []series
	for i := 0; i < 4; i++ {
		s = append(s, series{labels: []label{{"__name__", "m"}, {"i", string(rune('a' + i))}}})
	}
	for _, ts := range rnd.Perm(n) {
		i := rnd.Intn(len(s))
		s[i].points = append(s[i].points, point{float64(ts + 1), int64(ts + 1)})
	}
	for _, x := range s {
		sort.Slice(x.points, func(a, b int) bool { return x.points[a].tsMs < x.points[b].tsMs })
	}
	err := NewClient(rx.URL, testClient).write(t.Context(), s)
	rej, ok := err.(*RejectedError)
	if !ok || rej.Dropped != cutoff-1 {
		t.Fatalf("err = %v, want %d dropped", err, cutoff-1)
	}
	if len(p.stored) != n-cutoff+1 {
		t.Errorf("stored %d samples, want %d", len(p.stored), n-cutoff+1)
	}
	for ts := int64(cutoff); ts <= n; ts++ {
		if v, ok := p.stored[ts]; !ok || v != float64(ts) {
			t.Fatalf("sample %d missing or wrong (%v)", ts, v)
		}
	}
	if p.calls > 2+14 {
		t.Errorf("%d upstream calls, want O(log n) (<= 16)", p.calls)
	}
}
