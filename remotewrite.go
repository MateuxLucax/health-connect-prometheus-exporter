package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
)

const maxSamplesPerRequest = 10000

type label struct{ name, value string }

type point struct {
	value float64
	tsMs  int64
}

type series struct {
	labels []label // sorted by name, including __name__
	points []point // sorted by timestamp, unique timestamps
}

// groupSeries groups samples into series by metric name and labels. Within a
// series, points are sorted by timestamp and for duplicate timestamps the last
// sample in the request wins. Labels with an empty value are dropped, as an
// empty value means "no label" in Prometheus.
func groupSeries(samples []sample) []series {
	type acc struct {
		labels []label
		byTs   map[int64]float64
	}
	groups := map[string]*acc{}
	for _, s := range samples {
		labels := []label{{"__name__", s.Metric}}
		for n, v := range s.Labels {
			if v != "" {
				labels = append(labels, label{n, v})
			}
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i].name < labels[j].name })
		var key strings.Builder
		for _, l := range labels {
			key.WriteString(l.name + "\xff" + l.value + "\xff")
		}
		g := groups[key.String()]
		if g == nil {
			g = &acc{labels: labels, byTs: map[int64]float64{}}
			groups[key.String()] = g
		}
		g.byTs[s.TimestampMs] = *s.Value
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]series, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		points := make([]point, 0, len(g.byTs))
		for ts, v := range g.byTs {
			points = append(points, point{v, ts})
		}
		sort.Slice(points, func(i, j int) bool { return points[i].tsMs < points[j].tsMs })
		out = append(out, series{g.labels, points})
	}
	return out
}

// splitBatches splits series so that no batch holds more than max points.
func splitBatches(all []series, max int) [][]series {
	var out [][]series
	var cur []series
	n := 0
	for _, s := range all {
		for len(s.points) > 0 {
			take := min(max-n, len(s.points))
			cur = append(cur, series{s.labels, s.points[:take]})
			s.points = s.points[take:]
			n += take
			if n == max {
				out = append(out, cur)
				cur, n = nil, 0
			}
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// rejectedError is a rejection by the receiver that retrying will not fix:
// HTTP 400, e.g. a sample outside the out-of-order window or a duplicate
// sample for a timestamp. Any other non-2xx status (404 for a disabled
// receiver, 401/403 from a proxy, 413, 429, 5xx) is a plain error, so the
// request fails and the app retries later instead of losing its data.
type rejectedError struct {
	dropped int // number of samples dropped
	msg     string
}

func (e *rejectedError) Error() string { return e.msg }

// isTooOld reports whether a rejection message from Prometheus means a sample
// is older than what the TSDB accepts ("too old sample" outside the
// out-of-order window, "out of bounds" before the head's minimum time).
func isTooOld(msg string) bool {
	return strings.Contains(msg, "too old sample") || strings.Contains(msg, "out of bounds")
}

type remoteWriter struct {
	url    string
	client *http.Client
}

// write sends all series in batches. It returns a *rejectedError if some
// samples were rejected for good (the rest were written), or another error if
// the receiver could not be reached and the whole request should be retried.
func (w *remoteWriter) write(ctx context.Context, all []series) error {
	rej := &rejectedError{}
	for _, batch := range splitBatches(all, maxSamplesPerRequest) {
		if err := w.sendIsolating(ctx, batch, rej); err != nil {
			return err
		}
	}
	if rej.dropped > 0 {
		return rej
	}
	return nil
}

// sendIsolating sends a batch. Prometheus discards the whole request when any
// one sample in it is rejected, so the offending samples are left out and the
// rest resent. They are counted in rej. Samples rejected for being too old are
// found by a binary search on the timestamp cutoff (O(log n) requests); for
// other rejections the batch is halved until only the offending samples are
// left out.
func (w *remoteWriter) sendIsolating(ctx context.Context, batch []series, rej *rejectedError) error {
	err := w.send(ctx, batch)
	var r *rejectedError
	if !errors.As(err, &r) {
		return err
	}
	n := 0
	for _, s := range batch {
		n += len(s.points)
	}
	if n == 1 {
		rej.dropped++
		rej.msg = r.msg
		return nil
	}
	if isTooOld(r.msg) {
		cutoff, err := w.findCutoff(ctx, batch)
		if err != nil {
			return err
		}
		kept, dropped := dropBefore(batch, cutoff)
		if dropped > 0 {
			rej.dropped += dropped
			rej.msg = r.msg
			if len(kept) == 0 {
				return nil
			}
			return w.sendIsolating(ctx, kept, rej)
		}
		// No sample is too old on its own; fall back to halving.
	}
	for _, half := range splitBatches(batch, (n+1)/2) {
		if err := w.sendIsolating(ctx, half, rej); err != nil {
			return err
		}
	}
	return nil
}

// findCutoff returns the oldest timestamp in batch that the receiver does not
// reject as too old, or math.MaxInt64 if it rejects all of them. It sends
// single samples, binary searching over the batch sorted by timestamp.
func (w *remoteWriter) findCutoff(ctx context.Context, batch []series) (int64, error) {
	var all []series
	for _, s := range batch {
		for i := range s.points {
			all = append(all, series{s.labels, s.points[i : i+1]})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].points[0].tsMs < all[j].points[0].tsMs })
	// Invariant: all[:lo] are too old, all[hi:] are not.
	lo, hi := 0, len(all)
	for lo < hi {
		mid := (lo + hi) / 2
		err := w.send(ctx, all[mid:mid+1])
		var r *rejectedError
		switch {
		case errors.As(err, &r) && isTooOld(r.msg):
			lo = mid + 1
		case err == nil || errors.As(err, &r):
			hi = mid
		default:
			return 0, err
		}
	}
	if lo == len(all) {
		return math.MaxInt64, nil
	}
	return all[lo].points[0].tsMs, nil
}

// dropBefore returns batch without the points older than cutoff, and how many
// points were removed.
func dropBefore(batch []series, cutoff int64) ([]series, int) {
	var kept []series
	dropped := 0
	for _, s := range batch {
		i := sort.Search(len(s.points), func(i int) bool { return s.points[i].tsMs >= cutoff })
		dropped += i
		if i < len(s.points) {
			kept = append(kept, series{s.labels, s.points[i:]})
		}
	}
	return kept, dropped
}

func (w *remoteWriter) send(ctx context.Context, batch []series) error {
	body := snappyEncode(encodeWriteRequest(batch))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return &rejectedError{msg: fmt.Sprintf("remote write rejected (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))}
	default:
		return fmt.Errorf("remote write failed (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
}

// encodeWriteRequest hand-encodes a prometheus.WriteRequest protobuf:
//
//	WriteRequest { repeated TimeSeries timeseries = 1; }
//	TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	Label        { string name = 1; string value = 2; }
//	Sample       { double value = 1; int64 timestamp = 2; }
func encodeWriteRequest(batch []series) []byte {
	var out []byte
	for _, s := range batch {
		var ts []byte
		for _, l := range s.labels {
			var lb []byte
			lb = appendBytesField(lb, 1, []byte(l.name))
			lb = appendBytesField(lb, 2, []byte(l.value))
			ts = appendBytesField(ts, 1, lb)
		}
		for _, p := range s.points {
			var sb []byte
			sb = binary.AppendUvarint(sb, 1<<3|1) // field 1, wire type fixed64
			sb = binary.LittleEndian.AppendUint64(sb, math.Float64bits(p.value))
			sb = binary.AppendUvarint(sb, 2<<3|0) // field 2, wire type varint
			sb = binary.AppendUvarint(sb, uint64(p.tsMs))
			ts = appendBytesField(ts, 2, sb)
		}
		out = appendBytesField(out, 1, ts)
	}
	return out
}

// appendBytesField appends a length-delimited (wire type 2) field.
func appendBytesField(b []byte, field uint64, v []byte) []byte {
	b = binary.AppendUvarint(b, field<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// snappyEncode encodes src in the snappy block format using only literal
// elements (valid snappy, just uncompressed).
func snappyEncode(src []byte) []byte {
	dst := binary.AppendUvarint(nil, uint64(len(src)))
	for len(src) > 0 {
		n := min(len(src), 64<<10)
		dst = appendSnappyLiteral(dst, src[:n])
		src = src[n:]
	}
	return dst
}

// appendSnappyLiteral appends a literal element. The tag's upper 6 bits hold
// len-1 when it is below 60; otherwise 60..63 say that len-1 follows in
// 1..4 little-endian bytes.
func appendSnappyLiteral(dst, lit []byte) []byte {
	n := uint32(len(lit) - 1)
	switch {
	case n < 60:
		dst = append(dst, byte(n)<<2)
	case n < 1<<8:
		dst = append(dst, 60<<2, byte(n))
	case n < 1<<16:
		dst = append(dst, 61<<2, byte(n), byte(n>>8))
	case n < 1<<24:
		dst = append(dst, 62<<2, byte(n), byte(n>>8), byte(n>>16))
	default:
		dst = append(dst, 63<<2, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	}
	return append(dst, lit...)
}
