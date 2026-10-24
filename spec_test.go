package main

// Specification-driven tests. Everything sent to the fake Prometheus is decoded
// with a snappy and protobuf decoder written here from the public format
// descriptions, independent of the production encoder.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Snappy block format decoder (literals and all three copy element kinds).
// https://github.com/google/snappy/blob/main/format_description.txt

func specSnappyDecode(src []byte) ([]byte, error) {
	want, n := binary.Uvarint(src)
	if n <= 0 {
		return nil, errors.New("snappy: bad length preamble")
	}
	if want > 1<<30 {
		return nil, errors.New("snappy: length too large")
	}
	src = src[n:]
	dst := make([]byte, 0, want)
	for len(src) > 0 {
		tag := src[0]
		src = src[1:]
		switch tag & 3 {
		case 0: // literal
			l := uint64(tag >> 2)
			if l >= 60 {
				nb := int(l - 59)
				if len(src) < nb {
					return nil, errors.New("snappy: short literal length")
				}
				l = 0
				for i := 0; i < nb; i++ {
					l |= uint64(src[i]) << (8 * i)
				}
				src = src[nb:]
			}
			l++
			if uint64(len(src)) < l {
				return nil, errors.New("snappy: short literal")
			}
			dst = append(dst, src[:l]...)
			src = src[l:]
		default:
			var length, offset int
			switch tag & 3 {
			case 1:
				if len(src) < 1 {
					return nil, errors.New("snappy: short copy1")
				}
				length = 4 + int(tag>>2&7)
				offset = int(tag>>5)<<8 | int(src[0])
				src = src[1:]
			case 2:
				if len(src) < 2 {
					return nil, errors.New("snappy: short copy2")
				}
				length = 1 + int(tag>>2)
				offset = int(src[0]) | int(src[1])<<8
				src = src[2:]
			case 3:
				if len(src) < 4 {
					return nil, errors.New("snappy: short copy4")
				}
				length = 1 + int(tag>>2)
				offset = int(binary.LittleEndian.Uint32(src))
				src = src[4:]
			}
			if offset == 0 || offset > len(dst) {
				return nil, fmt.Errorf("snappy: bad copy offset %d (have %d)", offset, len(dst))
			}
			start := len(dst) - offset
			for i := 0; i < length; i++ { // byte by byte: copies may overlap
				dst = append(dst, dst[start+i])
			}
		}
	}
	if uint64(len(dst)) != want {
		return nil, fmt.Errorf("snappy: decoded %d bytes, preamble says %d", len(dst), want)
	}
	return dst, nil
}

func TestSpecSnappyDecoderSelfCheck(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"literal", []byte{5, 4 << 2, 'h', 'e', 'l', 'l', 'o'}, "hello"},
		// "abc" literal + copy1 (len 9, offset 3) -> overlapping copy.
		{"copy1 overlap", []byte{12, 2 << 2, 'a', 'b', 'c', (9-4)<<2 | 1, 3}, "abcabcabcabc"},
		// "ab" literal + copy2 (len 4, offset 2).
		{"copy2", []byte{6, 1 << 2, 'a', 'b', (4-1)<<2 | 2, 2, 0}, "ababab"},
		// "xyz" literal + copy4 (len 3, offset 3).
		{"copy4", []byte{6, 2 << 2, 'x', 'y', 'z', (3-1)<<2 | 3, 3, 0, 0, 0}, "xyzxyz"},
		// literal with one extra length byte (tag 60): 61 bytes.
		{"long literal", append([]byte{61, 60 << 2, 60}, bytes.Repeat([]byte{'q'}, 61)...), strings.Repeat("q", 61)},
	}
	for _, c := range cases {
		got, err := specSnappyDecode(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	if _, err := specSnappyDecode([]byte{4, 0, 'a', 1<<2 | 1, 5}); err == nil {
		t.Error("expected error for out-of-range copy offset")
	}
}

// ---------------------------------------------------------------------------
// Protobuf wire format decoder for prometheus.WriteRequest.

type specLabel struct{ Name, Value string }

type specSample struct {
	Value float64
	Ts    int64
}

type specSeries struct {
	Labels  []specLabel
	Samples []specSample
}

type pbField struct {
	num  uint64
	wire uint64
	v    uint64 // varint / fixed64 / fixed32
	b    []byte // length-delimited
}

func pbFields(b []byte) ([]pbField, error) {
	var out []pbField
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("pb: bad key")
		}
		b = b[n:]
		f := pbField{num: key >> 3, wire: key & 7}
		switch f.wire {
		case 0:
			f.v, n = binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("pb: bad varint")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("pb: short fixed64")
			}
			f.v = binary.LittleEndian.Uint64(b)
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("pb: bad length-delimited field")
			}
			f.b = b[n : n+int(l)]
			b = b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("pb: short fixed32")
			}
			f.v = uint64(binary.LittleEndian.Uint32(b))
			b = b[4:]
		default:
			return nil, fmt.Errorf("pb: unsupported wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}

func specDecodeWriteRequest(b []byte) ([]specSeries, error) {
	top, err := pbFields(b)
	if err != nil {
		return nil, err
	}
	var out []specSeries
	for _, tf := range top {
		if tf.num != 1 {
			continue
		}
		if tf.wire != 2 {
			return nil, errors.New("pb: timeseries not length-delimited")
		}
		fs, err := pbFields(tf.b)
		if err != nil {
			return nil, err
		}
		var s specSeries
		for _, f := range fs {
			switch f.num {
			case 1: // Label
				if f.wire != 2 {
					return nil, errors.New("pb: label wire type")
				}
				lfs, err := pbFields(f.b)
				if err != nil {
					return nil, err
				}
				var l specLabel
				for _, lf := range lfs {
					switch {
					case lf.num == 1 && lf.wire == 2:
						l.Name = string(lf.b)
					case lf.num == 2 && lf.wire == 2:
						l.Value = string(lf.b)
					}
				}
				s.Labels = append(s.Labels, l)
			case 2: // Sample
				if f.wire != 2 {
					return nil, errors.New("pb: sample wire type")
				}
				sfs, err := pbFields(f.b)
				if err != nil {
					return nil, err
				}
				var sm specSample
				for _, sf := range sfs {
					switch {
					case sf.num == 1 && sf.wire == 1:
						sm.Value = math.Float64frombits(sf.v)
					case sf.num == 2 && sf.wire == 0:
						sm.Ts = int64(sf.v)
					}
				}
				s.Samples = append(s.Samples, sm)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fake Prometheus.

type fakeRequest struct {
	header http.Header
	series []specSeries
}

func (r fakeRequest) sampleCount() int {
	n := 0
	for _, s := range r.series {
		n += len(s.Samples)
	}
	return n
}

type fakeProm struct {
	t   *testing.T
	srv *httptest.Server

	// decide returns the HTTP status for a decoded request (default 204).
	decide func([]specSeries) int

	mu       sync.Mutex
	requests []fakeRequest
	stored   map[string]float64 // seriesKey + "@" + ts -> value
}

func specSeriesKey(ls []specLabel) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Name + "=" + l.Value + ";")
	}
	return b.String()
}

func specPointKey(ls []specLabel, ts int64) string {
	return fmt.Sprintf("%s@%d", specSeriesKey(ls), ts)
}

func newFakeProm(t *testing.T, decide func([]specSeries) int) *fakeProm {
	fp := &fakeProm{t: t, decide: decide, stored: map[string]float64{}}
	fp.srv = httptest.NewServer(http.HandlerFunc(fp.serve))
	t.Cleanup(fp.srv.Close)
	return fp
}

func (fp *fakeProm) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fp.t.Errorf("fake prom: read body: %v", err)
		w.WriteHeader(500)
		return
	}
	raw, err := specSnappyDecode(body)
	if err != nil {
		fp.t.Errorf("fake prom: %v", err)
		w.WriteHeader(500)
		return
	}
	ss, err := specDecodeWriteRequest(raw)
	if err != nil {
		fp.t.Errorf("fake prom: %v", err)
		w.WriteHeader(500)
		return
	}
	status := http.StatusNoContent
	if fp.decide != nil {
		status = fp.decide(ss)
	}
	fp.mu.Lock()
	fp.requests = append(fp.requests, fakeRequest{header: r.Header.Clone(), series: ss})
	if status/100 == 2 {
		for _, s := range ss {
			for _, sm := range s.Samples {
				fp.stored[specPointKey(s.Labels, sm.Ts)] = sm.Value
			}
		}
	}
	fp.mu.Unlock()
	w.WriteHeader(status)
}

func (fp *fakeProm) snapshot() ([]fakeRequest, map[string]float64) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	st := make(map[string]float64, len(fp.stored))
	for k, v := range fp.stored {
		st[k] = v
	}
	return append([]fakeRequest(nil), fp.requests...), st
}

// ---------------------------------------------------------------------------
// Helpers to drive the exporter handler.

const specToken = "s3cr3t-token"

type specIn struct {
	Metric      string            `json:"metric"`
	Labels      map[string]string `json:"labels,omitempty"`
	Value       float64           `json:"value"`
	TimestampMs int64             `json:"timestamp_ms"`
}

func specBody(t *testing.T, in []specIn) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"samples": in})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func specHandler(url string) http.Handler {
	return newHandler(specToken, &remoteWriter{url: url, client: &http.Client{Timeout: 10 * time.Second}})
}

func specPost(h http.Handler, auth string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/samples", body)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func specPostOK(t *testing.T, h http.Handler, body string) {
	t.Helper()
	rec := specPost(h, "Bearer "+specToken, strings.NewReader(body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (%s)", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// E1 Auth

func TestSpecAuth(t *testing.T) {
	fp := newFakeProm(t, nil)
	h := specHandler(fp.srv.URL)
	body := `{"samples":[{"metric":"m","value":1,"timestamp_ms":1000}]}`
	cases := []struct {
		name, auth string
		want       int
	}{
		{"no header", "", 401},
		{"wrong token", "Bearer wrong-token!", 401},
		{"basic scheme", "Basic " + specToken, 401},
		{"shorter token", "Bearer " + specToken[:3], 401},
		{"longer token", "Bearer " + specToken + "x", 401},
		{"bare token", specToken, 401},
		{"correct", "Bearer " + specToken, 204},
	}
	for _, c := range cases {
		rec := specPost(h, c.auth, strings.NewReader(body))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
	reqs, _ := fp.snapshot()
	if len(reqs) != 1 {
		t.Errorf("upstream calls = %d, want 1 (only the authorized request)", len(reqs))
	}
}

// ---------------------------------------------------------------------------
// E2 Routing

func TestSpecRouting(t *testing.T) {
	fp := newFakeProm(t, nil)
	h := specHandler(fp.srv.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/samples", nil)
	req.Header.Set("Authorization", "Bearer "+specToken)
	h.ServeHTTP(rec, req)
	if rec.Code != 405 && rec.Code != 404 {
		t.Errorf("GET /api/v1/samples: status %d, want 405 or 404", rec.Code)
	}
	t.Logf("GET /api/v1/samples -> %d", rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != 404 {
		t.Errorf("GET /nope: status %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("GET /healthz: %d %q, want 200 \"ok\"", rec.Code, rec.Body)
	}
	if reqs, _ := fp.snapshot(); len(reqs) != 0 {
		t.Errorf("upstream calls = %d, want 0", len(reqs))
	}
}

// ---------------------------------------------------------------------------
// E3 Validation

func TestSpecValidation(t *testing.T) {
	sampleWith := func(fields string) string {
		return `{"samples":[{"metric":"ok_metric","value":1,"timestamp_ms":1000},` + fields + `]}`
	}
	cases := []struct {
		name string
		body string
	}{
		{"not json", `this is not json`},
		{"truncated json", `{"samples":[{"metric":"m","value":1,`},
		{"empty body", ``},
		{"trailing garbage", `{"samples":[{"metric":"m","value":1,"timestamp_ms":1000}]} trailing`},
		{"samples not array", `{"samples":{"metric":"m"}}`},
		{"value is string", sampleWith(`{"metric":"m","value":"1","timestamp_ms":1000}`)},
		{"metric starts with digit", sampleWith(`{"metric":"1abc","value":1,"timestamp_ms":1000}`)},
		{"metric with dash", sampleWith(`{"metric":"a-b","value":1,"timestamp_ms":1000}`)},
		{"empty metric", sampleWith(`{"metric":"","value":1,"timestamp_ms":1000}`)},
		{"missing metric", sampleWith(`{"value":1,"timestamp_ms":1000}`)},
		{"label __name__", sampleWith(`{"metric":"m","labels":{"__name__":"x"},"value":1,"timestamp_ms":1000}`)},
		{"label __foo", sampleWith(`{"metric":"m","labels":{"__foo":"x"},"value":1,"timestamp_ms":1000}`)},
		{"label starts with digit", sampleWith(`{"metric":"m","labels":{"1x":"x"},"value":1,"timestamp_ms":1000}`)},
		{"label with dash", sampleWith(`{"metric":"m","labels":{"a-b":"x"},"value":1,"timestamp_ms":1000}`)},
		{"empty label name", sampleWith(`{"metric":"m","labels":{"":"x"},"value":1,"timestamp_ms":1000}`)},
		{"timestamp zero", sampleWith(`{"metric":"m","value":1,"timestamp_ms":0}`)},
		{"timestamp negative", sampleWith(`{"metric":"m","value":1,"timestamp_ms":-1000}`)},
		{"timestamp missing", sampleWith(`{"metric":"m","value":1}`)},
		{"value missing", sampleWith(`{"metric":"m","timestamp_ms":1000}`)},
		{"value null", sampleWith(`{"metric":"m","value":null,"timestamp_ms":1000}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := newFakeProm(t, nil)
			h := specHandler(fp.srv.URL)
			rec := specPost(h, "Bearer "+specToken, strings.NewReader(c.body))
			if rec.Code != 400 {
				t.Errorf("status %d, want 400 (%s)", rec.Code, rec.Body)
			}
			if reqs, _ := fp.snapshot(); len(reqs) != 0 {
				t.Errorf("upstream calls = %d, want 0", len(reqs))
			}
		})
	}

	// Valid edge cases that must be accepted.
	ok := []string{
		`{"samples":[{"metric":"a:b_c","labels":{"_x":"1","X9":"2"},"value":1,"timestamp_ms":1}]}`,
		`{"samples":[{"metric":"_m","labels":{"_":"v"},"value":0,"timestamp_ms":1}]}`,
		`{"samples":[{"metric":"m","labels":{},"value":-1,"timestamp_ms":1}]}`,
	}
	for _, body := range ok {
		fp := newFakeProm(t, nil)
		h := specHandler(fp.srv.URL)
		if rec := specPost(h, "Bearer "+specToken, strings.NewReader(body)); rec.Code != 204 {
			t.Errorf("valid body %s: status %d, want 204 (%s)", body, rec.Code, rec.Body)
		}
	}
}

func TestSpecBodyTooLarge(t *testing.T) {
	fp := newFakeProm(t, nil)
	h := specHandler(fp.srv.URL)
	// Syntactically valid JSON whose size exceeds 32 MiB.
	big := strings.Repeat("a", 32<<20)
	body := `{"samples":[{"metric":"m","labels":{"big":"` + big + `"},"value":1,"timestamp_ms":1000}]}`
	if len(body) <= 32<<20 {
		t.Fatal("body not large enough")
	}
	rec := specPost(h, "Bearer "+specToken, strings.NewReader(body))
	if rec.Code != 400 {
		t.Errorf("status %d, want 400", rec.Code)
	}
	if reqs, _ := fp.snapshot(); len(reqs) != 0 {
		t.Errorf("upstream calls = %d, want 0", len(reqs))
	}
}

// ---------------------------------------------------------------------------
// E4 Empty list

func TestSpecEmptySamples(t *testing.T) {
	fp := newFakeProm(t, func([]specSeries) int { return 500 })
	h := specHandler(fp.srv.URL)
	rec := specPost(h, "Bearer "+specToken, strings.NewReader(`{"samples":[]}`))
	if rec.Code != 204 {
		t.Errorf("status %d, want 204", rec.Code)
	}
	if reqs, _ := fp.snapshot(); len(reqs) != 0 {
		t.Errorf("upstream calls = %d, want 0", len(reqs))
	}
}

// ---------------------------------------------------------------------------
// E5 Fidelity

func TestSpecFidelity(t *testing.T) {
	fp := newFakeProm(t, nil)
	h := specHandler(fp.srv.URL)
	in := []specIn{
		{"health_heart_rate_bpm", map[string]string{"source": "x", "device": "Pixel ⌚ 日本語", "zone": "café"}, 72.5, 1792839600000},
		{"health_heart_rate_bpm", map[string]string{"zone": "café", "source": "x", "device": "Pixel ⌚ 日本語"}, 0, 1792839600001},
		{"health_heart_rate_bpm", map[string]string{"device": "Pixel ⌚ 日本語", "source": "x", "zone": "café"}, -3.25, 1792839600002},
		{"health_heart_rate_bpm", map[string]string{"source": "x", "zone": "café", "device": "Pixel ⌚ 日本語"}, 1e-9, 1792839600003},
		{"health_heart_rate_bpm", map[string]string{"source": "x", "device": "Pixel ⌚ 日本語", "zone": "café"}, 1e308, 9007199254740993}, // > 2^53
	}
	specPostOK(t, h, specBody(t, in))

	reqs, _ := fp.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(reqs))
	}
	hd := reqs[0].header
	for k, v := range map[string]string{
		"Content-Encoding":                  "snappy",
		"Content-Type":                      "application/x-protobuf",
		"X-Prometheus-Remote-Write-Version": "0.1.0",
	} {
		if got := hd.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	want := []specSeries{{
		Labels: []specLabel{
			{"__name__", "health_heart_rate_bpm"},
			{"device", "Pixel ⌚ 日本語"},
			{"source", "x"},
			{"zone", "café"},
		},
		Samples: []specSample{
			{72.5, 1792839600000},
			{0, 1792839600001},
			{-3.25, 1792839600002},
			{1e-9, 1792839600003},
			{1e308, 9007199254740993},
		},
	}}
	got := reqs[0].series
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded series\n got %+v\nwant %+v", got, want)
	}
	for i, s := range got[0].Samples {
		if math.Float64bits(s.Value) != math.Float64bits(want[0].Samples[i].Value) {
			t.Errorf("sample %d: value bits differ: %v vs %v", i, s.Value, want[0].Samples[i].Value)
		}
	}
}

// ---------------------------------------------------------------------------
// E6 Grouping

func TestSpecGrouping(t *testing.T) {
	fp := newFakeProm(t, nil)
	h := specHandler(fp.srv.URL)
	A := map[string]string{"source": "a"}
	B := map[string]string{"source": "b"}
	in := []specIn{
		{"m1", A, 3, 300},
		{"m2", nil, 20, 200},
		{"m1", B, 1, 100},
		{"m1", A, 1, 100},
		{"m2", nil, 10, 100},
		{"m1", B, 3, 300},
		{"m1", A, 2, 200},
		{"m1", B, 2, 200},
		{"m2", nil, 30, 300},
		{"m1", A, 99, 200}, // duplicate ts in series m1{source=a}: last wins
		{"m2", map[string]string{"source": ""}, 40, 400},             // empty value dropped -> merges with m2{}
		{"m2", map[string]string{"source": "", "other": ""}, 50, 50}, // same
	}
	specPostOK(t, h, specBody(t, in))

	reqs, _ := fp.snapshot()
	var all []specSeries
	for _, r := range reqs {
		all = append(all, r.series...)
	}
	sort.Slice(all, func(i, j int) bool { return specSeriesKey(all[i].Labels) < specSeriesKey(all[j].Labels) })
	want := []specSeries{
		{Labels: []specLabel{{"__name__", "m1"}, {"source", "a"}}, Samples: []specSample{{1, 100}, {99, 200}, {3, 300}}},
		{Labels: []specLabel{{"__name__", "m1"}, {"source", "b"}}, Samples: []specSample{{1, 100}, {2, 200}, {3, 300}}},
		{Labels: []specLabel{{"__name__", "m2"}}, Samples: []specSample{{50, 50}, {10, 100}, {20, 200}, {30, 300}, {40, 400}}},
	}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("series\n got %+v\nwant %+v", all, want)
	}
}

// ---------------------------------------------------------------------------
// E7 Batching

func TestSpecBatching(t *testing.T) {
	fp := newFakeProm(t, nil)
	h := specHandler(fp.srv.URL)
	const n = 25000
	in := make([]specIn, 0, n)
	want := map[string]float64{}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("s%d", i%3)
		ts := int64(1_700_000_000_000 + i*1000)
		v := float64(i) + 0.5
		in = append(in, specIn{"health_steps", map[string]string{"source": src}, v, ts})
		want[specPointKey([]specLabel{{"__name__", "health_steps"}, {"source", src}}, ts)] = v
	}
	specPostOK(t, h, specBody(t, in))

	reqs, stored := fp.snapshot()
	if len(reqs) < 3 {
		t.Errorf("upstream calls = %d, want >= 3", len(reqs))
	}
	total := 0
	for i, r := range reqs {
		c := r.sampleCount()
		total += c
		if c > 10000 {
			t.Errorf("request %d carries %d samples, want <= 10000", i, c)
		}
		for _, s := range r.series {
			for j := 1; j < len(s.Samples); j++ {
				if s.Samples[j-1].Ts >= s.Samples[j].Ts {
					t.Fatalf("request %d: samples not strictly sorted by timestamp", i)
				}
			}
		}
	}
	if total != n {
		t.Errorf("total samples sent = %d, want %d (no duplicates, no losses)", total, n)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Errorf("stored %d points, want %d; contents differ", len(stored), len(want))
	}
}

// ---------------------------------------------------------------------------
// E8 Upstream errors

func TestSpecUpstreamErrors(t *testing.T) {
	body := `{"samples":[{"metric":"m","value":1,"timestamp_ms":1000},{"metric":"m","value":2,"timestamp_ms":2000}]}`
	for _, c := range []struct {
		status, want int
	}{
		{400, 204}, {404, 204}, {413, 204}, {422, 204},
		{429, 502}, {500, 502}, {503, 502},
	} {
		fp := newFakeProm(t, func([]specSeries) int { return c.status })
		h := specHandler(fp.srv.URL)
		rec := specPost(h, "Bearer "+specToken, strings.NewReader(body))
		if rec.Code != c.want {
			t.Errorf("upstream %d: status %d, want %d", c.status, rec.Code, c.want)
		}
	}

	// Unreachable: closed server.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	rec := specPost(specHandler(url), "Bearer "+specToken, strings.NewReader(body))
	if rec.Code != 502 {
		t.Errorf("unreachable upstream: status %d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// E9 Partial rejection

func TestSpecPartialRejection(t *testing.T) {
	const threshold = int64(1_700_000_000_000)
	fp := newFakeProm(t, func(ss []specSeries) int {
		for _, s := range ss {
			for _, sm := range s.Samples {
				if sm.Ts < threshold {
					return 400
				}
			}
		}
		return 204
	})
	h := specHandler(fp.srv.URL)

	const n = 15000
	in := make([]specIn, 0, n)
	wantStored := map[string]float64{}
	old := 0
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("s%d", i%4)
		ts := threshold + int64(i)*1000
		if i%2111 == 7 { // sprinkle a few old samples across batches and series
			ts = threshold - int64(i+1)*1000
			old++
		}
		v := float64(i)
		in = append(in, specIn{"health_hr", map[string]string{"source": src}, v, ts})
		if ts >= threshold {
			wantStored[specPointKey([]specLabel{{"__name__", "health_hr"}, {"source", src}}, ts)] = v
		}
	}
	if old == 0 {
		t.Fatal("test setup: no old samples")
	}
	specPostOK(t, h, specBody(t, in))

	_, stored := fp.snapshot()
	for k, v := range stored {
		if wv, ok := wantStored[k]; !ok || wv != v {
			t.Fatalf("unexpected stored point %s=%v", k, v)
		}
	}
	if len(stored) != len(wantStored) {
		t.Errorf("stored %d points, want %d (all valid ones)", len(stored), len(wantStored))
	}
}

// ---------------------------------------------------------------------------
// E11 Concurrency

func TestSpecConcurrency(t *testing.T) {
	fp := newFakeProm(t, nil)
	srv := httptest.NewServer(specHandler(fp.srv.URL))
	defer srv.Close()

	const workers, per = 20, 50
	want := map[string]float64{}
	bodies := make([]string, workers)
	for w := 0; w < workers; w++ {
		in := make([]specIn, per)
		for i := range in {
			ts := int64(1_700_000_000_000 + i*1000)
			v := float64(w*1000 + i)
			lbl := fmt.Sprintf("r%d", w)
			in[i] = specIn{"health_conc", map[string]string{"req": lbl}, v, ts}
			want[specPointKey([]specLabel{{"__name__", "health_conc"}, {"req", lbl}}, ts)] = v
		}
		bodies[w] = specBody(t, in)
	}

	var wg sync.WaitGroup
	codes := make([]int, workers)
	errs := make([]error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/samples", strings.NewReader(bodies[w]))
			req.Header.Set("Authorization", "Bearer "+specToken)
			resp, err := srv.Client().Do(req)
			if err != nil {
				errs[w] = err
				return
			}
			resp.Body.Close()
			codes[w] = resp.StatusCode
		}(w)
	}
	wg.Wait()
	for w := 0; w < workers; w++ {
		if errs[w] != nil || codes[w] != 204 {
			t.Errorf("worker %d: status %d, err %v", w, codes[w], errs[w])
		}
	}
	_, stored := fp.snapshot()
	if !reflect.DeepEqual(stored, want) {
		t.Errorf("stored %d points, want %d", len(stored), len(want))
	}
}

// ---------------------------------------------------------------------------
// E10 Startup (real binary)

func specEnv(kv ...string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "AUTH_TOKEN=") || strings.HasPrefix(e, "REMOTE_WRITE_URL=") || strings.HasPrefix(e, "LISTEN_ADDR=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, kv...)
}

func specFreeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestSpecStartup(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "exporter")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	for _, c := range []struct {
		name string
		env  []string
	}{
		{"no AUTH_TOKEN", specEnv("REMOTE_WRITE_URL=http://127.0.0.1:1/api/v1/write", "LISTEN_ADDR="+specFreeAddr(t))},
		{"empty AUTH_TOKEN", specEnv("AUTH_TOKEN=", "REMOTE_WRITE_URL=http://127.0.0.1:1/api/v1/write", "LISTEN_ADDR="+specFreeAddr(t))},
		{"no REMOTE_WRITE_URL", specEnv("AUTH_TOKEN=x", "LISTEN_ADDR="+specFreeAddr(t))},
		{"empty REMOTE_WRITE_URL", specEnv("AUTH_TOKEN=x", "REMOTE_WRITE_URL=", "LISTEN_ADDR="+specFreeAddr(t))},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = c.env
		err := cmd.Run()
		timedOut := ctx.Err() != nil
		cancel()
		var ee *exec.ExitError
		if timedOut {
			t.Errorf("%s: process kept running, want non-zero exit", c.name)
		} else if !errors.As(err, &ee) || ee.ExitCode() == 0 {
			t.Errorf("%s: err %v, want non-zero exit", c.name, err)
		}
	}

	// Both set: serves /healthz and forwards end to end.
	fp := newFakeProm(t, nil)
	addr := specFreeAddr(t)
	cmd := exec.Command(bin)
	cmd.Env = specEnv("AUTH_TOKEN="+specToken, "REMOTE_WRITE_URL="+fp.srv.URL, "LISTEN_ADDR="+addr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	base := "http://" + addr
	var resp *http.Response
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(base + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("healthz never answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok" {
		t.Errorf("healthz: %d %q", resp.StatusCode, b)
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/samples",
		strings.NewReader(`{"samples":[{"metric":"health_heart_rate_bpm","labels":{"source":"x"},"value":72,"timestamp_ms":1792839600000}]}`))
	req.Header.Set("Authorization", "Bearer "+specToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("POST via binary: status %d, want 204", resp.StatusCode)
	}
	_, stored := fp.snapshot()
	k := specPointKey([]specLabel{{"__name__", "health_heart_rate_bpm"}, {"source", "x"}}, 1792839600000)
	if v, ok := stored[k]; !ok || v != 72 {
		t.Errorf("stored = %v, want %s=72", stored, k)
	}
}
