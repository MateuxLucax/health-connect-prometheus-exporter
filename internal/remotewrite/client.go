// Package remotewrite is a Prometheus remote write 1.0 client. It groups
// samples into series, splits them into batches, encodes them as snappy
// compressed protobuf and leaves out samples the receiver rejects for good.
// It uses only the standard library.
package remotewrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
)

const maxSamplesPerRequest = 10000

// RejectedError is a rejection by the receiver that retrying will not fix:
// HTTP 400, e.g. a sample outside the out-of-order window or a duplicate
// sample for a timestamp. Any other non-2xx status (404 for a disabled
// receiver, 401/403 from a proxy, 413, 429, 5xx) is a plain error, so the
// request fails and the app retries later instead of losing its data.
//
// Only this package creates a RejectedError; callers read it with errors.As.
// Dropped is the number of samples left out.
type RejectedError struct {
	Dropped int // number of samples dropped
	msg     string
}

func (e *RejectedError) Error() string { return e.msg }

// isTooOld reports whether a rejection message from Prometheus means a sample
// is older than what the TSDB accepts ("too old sample" outside the
// out-of-order window, "out of bounds" before the head's minimum time).
func isTooOld(msg string) bool {
	return strings.Contains(msg, "too old sample") || strings.Contains(msg, "out of bounds")
}

// Client writes samples to one remote write endpoint. It is safe for
// concurrent use.
type Client struct {
	url    string
	client *http.Client
}

// NewClient returns a Client that posts to url using client.
func NewClient(url string, client *http.Client) *Client {
	return &Client{url: url, client: client}
}

// Write groups samples into series and sends them in batches. It returns a
// *RejectedError if some samples were rejected for good (the rest were
// written), or another error if the receiver could not be reached and the
// whole request should be retried.
//
// Write does not validate samples: they must already satisfy the naming rules
// described on Sample.
func (c *Client) Write(ctx context.Context, samples []Sample) error {
	return c.write(ctx, groupSeries(samples))
}

func (c *Client) write(ctx context.Context, all []series) error {
	rej := &RejectedError{}
	for _, batch := range splitBatches(all, maxSamplesPerRequest) {
		if err := c.sendIsolating(ctx, batch, rej); err != nil {
			return err
		}
	}
	if rej.Dropped > 0 {
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
func (c *Client) sendIsolating(ctx context.Context, batch []series, rej *RejectedError) error {
	err := c.send(ctx, batch)
	var r *RejectedError
	if !errors.As(err, &r) {
		return err
	}
	n := 0
	for _, s := range batch {
		n += len(s.points)
	}
	if n == 1 {
		rej.Dropped++
		rej.msg = r.msg
		return nil
	}
	if isTooOld(r.msg) {
		cutoff, err := c.findCutoff(ctx, batch)
		if err != nil {
			return err
		}
		kept, dropped := dropBefore(batch, cutoff)
		if dropped > 0 {
			rej.Dropped += dropped
			rej.msg = r.msg
			if len(kept) == 0 {
				return nil
			}
			return c.sendIsolating(ctx, kept, rej)
		}
		// No sample is too old on its own; fall back to halving.
	}
	for _, half := range splitBatches(batch, (n+1)/2) {
		if err := c.sendIsolating(ctx, half, rej); err != nil {
			return err
		}
	}
	return nil
}

// findCutoff returns the oldest timestamp in batch that the receiver does not
// reject as too old, or math.MaxInt64 if it rejects all of them. It sends
// single samples, binary searching over the batch sorted by timestamp.
func (c *Client) findCutoff(ctx context.Context, batch []series) (int64, error) {
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
		err := c.send(ctx, all[mid:mid+1])
		var r *RejectedError
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

func (c *Client) send(ctx context.Context, batch []series) error {
	body := snappyEncode(encodeWriteRequest(batch))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return &RejectedError{msg: fmt.Sprintf("remote write rejected (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))}
	default:
		return fmt.Errorf("remote write failed (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
}
