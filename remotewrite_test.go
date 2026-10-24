package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if err := (&remoteWriter{url: rx.URL, client: http.DefaultClient}).write(t.Context(), s); err != nil {
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
	err := (&remoteWriter{url: rx.URL, client: http.DefaultClient}).write(t.Context(), s)
	rej, ok := err.(*rejectedError)
	if !ok || rej.dropped != 2 {
		t.Fatalf("err = %v, want 2 dropped", err)
	}
	if want := []int64{1, 3, 5}; !reflect.DeepEqual(stored, want) {
		t.Errorf("stored %v, want %v", stored, want)
	}
}
