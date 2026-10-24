package main

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
)

func f(v float64) *float64 { return &v }

func TestGroupSeries(t *testing.T) {
	got := groupSeries([]sample{
		{Metric: "b", Labels: map[string]string{"z": "1", "a": "2"}, Value: f(1), TimestampMs: 30},
		{Metric: "b", Labels: map[string]string{"a": "2", "z": "1"}, Value: f(2), TimestampMs: 10},
		{Metric: "b", Labels: map[string]string{"a": "2", "z": "1"}, Value: f(3), TimestampMs: 30}, // duplicate ts: last wins
		{Metric: "a", Labels: map[string]string{"empty": ""}, Value: f(4), TimestampMs: 5},
		{Metric: "a", Value: f(5), TimestampMs: 6}, // same series as above
	})
	want := []series{
		{labels: []label{{"__name__", "a"}}, points: []point{{4, 5}, {5, 6}}},
		{labels: []label{{"__name__", "b"}, {"a", "2"}, {"z", "1"}}, points: []point{{2, 10}, {3, 30}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestSplitBatches(t *testing.T) {
	s := []series{
		{labels: []label{{"__name__", "a"}}, points: make([]point, 3)},
		{labels: []label{{"__name__", "b"}}, points: make([]point, 4)},
	}
	var sizes [][]int
	for _, b := range splitBatches(s, 3) {
		var n []int
		for _, x := range b {
			n = append(n, len(x.points))
		}
		sizes = append(sizes, n)
	}
	want := [][]int{{3}, {3}, {1}}
	if !reflect.DeepEqual(sizes, want) {
		t.Errorf("got %v, want %v", sizes, want)
	}
}

func TestEncodeWriteRequest(t *testing.T) {
	got := encodeWriteRequest([]series{{
		labels: []label{{"__name__", "m"}},
		points: []point{{1, 2}},
	}})
	want := []byte{
		0x0a, 28, // timeseries (field 1), 28 bytes
		0x0a, 13, // label (field 1), 13 bytes
		0x0a, 8, '_', '_', 'n', 'a', 'm', 'e', '_', '_', // name
		0x12, 1, 'm', // value
		0x12, 11, // sample (field 2), 11 bytes
		0x09, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f, // value 1.0 as fixed64
		0x10, 2, // timestamp 2 as varint
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got  % x\nwant % x", got, want)
	}
}

func TestSnappyLiteralTags(t *testing.T) {
	// Tag encodings from the snappy format description, section 2.1.
	cases := []struct {
		n      int
		header []byte
	}{
		{1, []byte{0x00}},
		{60, []byte{59 << 2}},
		{61, []byte{60 << 2, 60}},
		{256, []byte{60 << 2, 0xff}},
		{257, []byte{61 << 2, 0x00, 0x01}},
		{1 << 16, []byte{61 << 2, 0xff, 0xff}},
		{1<<16 + 1, []byte{62 << 2, 0x00, 0x00, 0x01}},
		{1<<24 + 1, []byte{63 << 2, 0x00, 0x00, 0x00, 0x01}},
	}
	for _, c := range cases {
		lit := bytes.Repeat([]byte{'x'}, c.n)
		got := appendSnappyLiteral(nil, lit)
		if !bytes.Equal(got[:len(c.header)], c.header) || !bytes.Equal(got[len(c.header):], lit) {
			t.Errorf("len %d: header % x, want % x", c.n, got[:len(c.header)], c.header)
		}
	}
}

func TestSnappyEncode(t *testing.T) {
	if got := snappyEncode(nil); !bytes.Equal(got, []byte{0x00}) {
		t.Errorf("empty: % x", got)
	}
	if got, want := snappyEncode([]byte("abc")), []byte{0x03, 0x08, 'a', 'b', 'c'}; !bytes.Equal(got, want) {
		t.Errorf("abc: % x, want % x", got, want)
	}
	// 64 KiB + 1 bytes: uvarint length, then a 64 KiB literal and a 1-byte literal.
	src := bytes.Repeat([]byte{'y'}, 1<<16+1)
	got := snappyEncode(src)
	var want []byte
	want = append(want, 0x81, 0x80, 0x04)
	want = append(want, 61<<2, 0xff, 0xff)
	want = append(want, src[:1<<16]...)
	want = append(want, 0x00, 'y')
	if !bytes.Equal(got, want) {
		t.Errorf("64KiB+1: mismatch (len %d, want %d)", len(got), len(want))
	}
}

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
	if err := (&remoteWriter{url: rx.URL, client: testClient}).write(t.Context(), s); err != nil {
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
	err := (&remoteWriter{url: rx.URL, client: testClient}).write(t.Context(), s)
	rej, ok := err.(*rejectedError)
	if !ok || rej.dropped != 2 {
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
	err := (&remoteWriter{url: rx.URL, client: testClient}).write(t.Context(), s)
	rej, ok := err.(*rejectedError)
	if !ok || rej.dropped != n {
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
	err := (&remoteWriter{url: rx.URL, client: testClient}).write(t.Context(), s)
	rej, ok := err.(*rejectedError)
	if !ok || rej.dropped != cutoff-1 {
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
