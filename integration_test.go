//go:build integration

package main

// Integration tests: the compiled server binary against a real Prometheus.
//
//	PROMETHEUS_URL=http://localhost:9090 go test -tags integration -count=1 ./...
//
// Prometheus must run with --web.enable-remote-write-receiver and
// storage.tsdb.out_of_order_time_window: 30d. Every run uses a unique "run"
// label, so runs against the same Prometheus do not interfere.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type itSample struct {
	Metric      string            `json:"metric"`
	Labels      map[string]string `json:"labels,omitempty"`
	Value       float64           `json:"value"`
	TimestampMs int64             `json:"timestamp_ms"`
}

type itPoint struct {
	TsMs  int64
	Value float64
}

type itSeries struct {
	Labels map[string]string
	Points []itPoint
}

func itStartServer(t *testing.T, bin, token, writeURL string) string {
	t.Helper()
	addr := specFreeAddr(t)
	var logs bytes.Buffer
	cmd := exec.Command(bin)
	cmd.Env = specEnv("AUTH_TOKEN="+token, "REMOTE_WRITE_URL="+writeURL, "LISTEN_ADDR="+addr)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	base := "http://" + addr
	for deadline := time.Now().Add(10 * time.Second); ; {
		resp, err := testClient.Get(base + "/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && string(b) == "ok" {
				return base
			}
			err = fmt.Errorf("status %d %q", resp.StatusCode, b)
		}
		select {
		case werr := <-exited:
			exited <- werr // for the cleanup
			t.Fatalf("server exited early: %v\n%s", werr, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered /healthz with 200 ok: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func itPost(t *testing.T, base, token string, samples []itSample) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{"samples": samples})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/samples", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// itQuery runs an instant query at atMs and returns the vector or matrix result.
func itQuery(t *testing.T, prom, query string, atMs int64) []itSeries {
	t.Helper()
	q := url.Values{"query": {query}, "time": {fmt.Sprintf("%d.%03d", atMs/1000, atMs%1000)}}
	resp, err := testClient.Get(prom + "/api/v1/query?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Status string
		Data   struct {
			Result []struct {
				Metric map[string]string
				Value  []json.Number
				Values [][]json.Number
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out.Status != "success" {
		t.Fatalf("query %q: %v %s", query, err, raw)
	}
	var res []itSeries
	for _, r := range out.Data.Result {
		s := itSeries{Labels: r.Metric}
		vals := r.Values
		if r.Value != nil {
			vals = append(vals, r.Value)
		}
		for _, v := range vals {
			s.Points = append(s.Points, itPoint{itParseMs(t, v[0].String()), itParseFloat(t, v[1].String())})
		}
		res = append(res, s)
	}
	return res
}

// itParseMs parses a Prometheus API timestamp ("1792839600.123") exactly.
func itParseMs(t *testing.T, s string) int64 {
	t.Helper()
	sec, frac, _ := strings.Cut(s, ".")
	if len(frac) > 3 {
		t.Fatalf("timestamp %q has sub-millisecond digits", s)
	}
	ms, err := strconv.ParseInt(sec+(frac + "000")[:3], 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return ms
}

func itParseFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("value %q: %v", s, err)
	}
	return f
}

func itKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for n := range labels {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s=%q,", n, labels[n])
	}
	return b.String()
}

// itSingle returns the single value of a one-element vector result.
func itSingle(t *testing.T, res []itSeries, what string) float64 {
	t.Helper()
	if len(res) != 1 || len(res[0].Points) != 1 {
		t.Fatalf("%s: got %+v, want one value", what, res)
	}
	return res[0].Points[0].Value
}

func TestIntegration(t *testing.T) {
	prom := strings.TrimRight(os.Getenv("PROMETHEUS_URL"), "/")
	if prom == "" {
		t.Skip("PROMETHEUS_URL not set")
	}
	bin := filepath.Join(t.TempDir(), "exporter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	rnd := make([]byte, 16)
	rand.Read(rnd)
	token := hex.EncodeToString(rnd)
	run := strconv.FormatInt(time.Now().UnixNano(), 10)
	base := itStartServer(t, bin, token, prom+"/api/v1/write")
	now := time.Now().UnixMilli()
	const hour, day = int64(time.Hour / time.Millisecond), int64(24 * time.Hour / time.Millisecond)

	t.Run("I1 round trip", func(t *testing.T) {
		watch := map[string]string{"run": run, "source": "com.sec.android.app.shealth", "device": "Galaxy Watch ⌚ Ünïcødé 日本語"}
		phone := map[string]string{"run": run, "source": "com.sec.android.app.shealth", "device": "phone", "empty": ""}
		other := map[string]string{"run": run}
		in := []itSample{
			{"it_rt_heart_rate_bpm", watch, 61.5, now - hour},
			{"it_rt_heart_rate_bpm", phone, 70, now},
			{"it_rt_weight_kg", other, 80.25, now - 3*day},
			{"it_rt_heart_rate_bpm", watch, 72, now},
			{"it_rt_heart_rate_bpm", phone, -0.001, now - 3*day},
			{"it_rt_heart_rate_bpm", watch, 58, now - 3*day},
			{"it_rt_weight_kg", other, 79.9, now - hour},
		}
		if code := itPost(t, base, token, in); code != 204 {
			t.Fatalf("POST: status %d, want 204", code)
		}

		want := map[string]*itSeries{}
		for _, s := range in {
			labels := map[string]string{"__name__": s.Metric}
			for n, v := range s.Labels {
				if v != "" {
					labels[n] = v
				}
			}
			k := itKey(labels)
			if want[k] == nil {
				want[k] = &itSeries{Labels: labels}
			}
			want[k].Points = append(want[k].Points, itPoint{s.TimestampMs, s.Value})
		}
		for _, s := range want {
			sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].TsMs < s.Points[j].TsMs })
		}

		got := itQuery(t, prom, fmt.Sprintf(`{__name__=~"it_rt_.*",run=%q}[4d]`, run), now+1000)
		if len(got) != len(want) {
			t.Errorf("got %d series, want %d: %+v", len(got), len(want), got)
		}
		for _, g := range got {
			w := want[itKey(g.Labels)]
			if w == nil {
				t.Errorf("unexpected series %v", g.Labels)
				continue
			}
			if !reflect.DeepEqual(g.Points, w.Points) {
				t.Errorf("series %v:\n got  %+v\n want %+v", g.Labels, g.Points, w.Points)
			}
		}
	})

	t.Run("I2 interval metric sum_over_time", func(t *testing.T) {
		start := time.UnixMilli(now).UTC().Truncate(24 * time.Hour).Add(-48 * time.Hour).UnixMilli()
		labels := map[string]string{"run": run}
		var in []itSample
		total, first := 0.0, 0.0
		for i := int64(0); i < 1440; i++ {
			v := float64(i%23 + 1)
			in = append(in, itSample{"it_steps", labels, v, start + i*60000})
			total += v
			if i == 0 {
				first = v
			}
		}
		if code := itPost(t, base, token, in); code != 204 {
			t.Fatalf("POST: status %d, want 204", code)
		}
		q := fmt.Sprintf(`sum_over_time(it_steps{run=%q}[1d])`, run)
		// Ranges are left-open: (t-1d, t]. Evaluating 1 ms before the next
		// midnight covers exactly the day's samples, stamped at interval start.
		if got := itSingle(t, itQuery(t, prom, q, start+day-1), "end of day - 1ms"); got != total {
			t.Errorf("sum at end of day - 1ms = %v, want %v", got, total)
		}
		// At midnight itself the first sample (00:00) falls out of the range.
		if got := itSingle(t, itQuery(t, prom, q, start+day), "midnight"); got != total-first {
			t.Errorf("sum at midnight = %v, want %v", got, total-first)
		}
		// "offset 1ms" evaluated at midnight covers the day that just ended.
		qo := fmt.Sprintf(`sum_over_time(it_steps{run=%q}[1d] offset 1ms)`, run)
		if got := itSingle(t, itQuery(t, prom, qo, start+day), "midnight offset 1ms"); got != total {
			t.Errorf("sum at midnight with offset 1ms = %v, want %v", got, total)
		}
	})

	t.Run("I3 too old mixed with valid", func(t *testing.T) {
		labels := map[string]string{"run": run}
		// Make sure the head's max time is now, so the old sample is out of window.
		if code := itPost(t, base, token, []itSample{{"it_old", labels, 1, now}}); code != 204 {
			t.Fatalf("POST: status %d, want 204", code)
		}
		in := []itSample{
			{"it_old", labels, 2, now - 60*day},
			{"it_old", labels, 3, now - hour},
			{"it_old_other", labels, 4, now - 90*day},
			{"it_old_other", labels, 5, now - 2*day},
		}
		if code := itPost(t, base, token, in); code != 204 {
			t.Fatalf("POST: status %d, want 204", code)
		}
		got := itQuery(t, prom, fmt.Sprintf(`{__name__=~"it_old.*",run=%q}[100d]`, run), now+1000)
		byName := map[string][]itPoint{}
		for _, s := range got {
			byName[s.Labels["__name__"]] = s.Points
		}
		want := map[string][]itPoint{
			"it_old":       {{now - hour, 3}, {now, 1}},
			"it_old_other": {{now - 2*day, 5}},
		}
		if !reflect.DeepEqual(byName, want) {
			t.Errorf("stored %+v, want %+v", byName, want)
		}
	})

	t.Run("I4 duplicate timestamp in later request", func(t *testing.T) {
		labels := map[string]string{"run": run}
		t1, t2, t3 := now-2000, now-1000, now
		if code := itPost(t, base, token, []itSample{{"it_dup", labels, 1, t1}, {"it_dup", labels, 2, t2}}); code != 204 {
			t.Fatalf("first POST: status %d, want 204", code)
		}
		if code := itPost(t, base, token, []itSample{{"it_dup", labels, 20, t2}, {"it_dup", labels, 3, t3}}); code != 204 {
			t.Fatalf("second POST: status %d, want 204", code)
		}
		got := itQuery(t, prom, fmt.Sprintf(`it_dup{run=%q}[1h]`, run), now+1000)
		want := []itPoint{{t1, 1}, {t2, 2}, {t3, 3}}
		if len(got) != 1 || !reflect.DeepEqual(got[0].Points, want) {
			t.Errorf("stored %+v, want %+v", got, want)
		}
	})

	t.Run("I5 large batch", func(t *testing.T) {
		const n = 30000
		labels := map[string]string{"run": run}
		start := now - 4*day
		in := make([]itSample, n)
		for i := range in {
			in[i] = itSample{"it_large", labels, float64(i), start + int64(i)*10000}
		}
		if code := itPost(t, base, token, in); code != 204 {
			t.Fatalf("POST: status %d, want 204", code)
		}
		q := fmt.Sprintf(`count_over_time(it_large{run=%q}[5d])`, run)
		if got := itSingle(t, itQuery(t, prom, q, now), "count"); got != n {
			t.Errorf("count_over_time = %v, want %d", got, n)
		}
	})

	t.Run("I6 Prometheus down", func(t *testing.T) {
		// Keep the port taken while the server starts, so the server cannot
		// pick it as its own listen address; close it to make it refuse.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := itStartServer(t, bin, token, "http://"+l.Addr().String()+"/api/v1/write")
		l.Close()
		if code := itPost(t, dead, token, []itSample{{"it_down", map[string]string{"run": run}, 1, now}}); code != 502 {
			t.Errorf("status %d, want 502", code)
		}
	})

	t.Run("I8 sample far in the future dropped", func(t *testing.T) {
		labels := map[string]string{"run": run}
		in := []itSample{{"it_future", labels, 1, now}, {"it_future", labels, 2, now + 2*hour}}
		if code := itPost(t, base, token, in); code != 204 {
			t.Fatalf("POST: status %d, want 204", code)
		}
		got := itQuery(t, prom, fmt.Sprintf(`it_future{run=%q}[4h]`, run), now+3*hour)
		if want := []itPoint{{now, 1}}; len(got) != 1 || !reflect.DeepEqual(got[0].Points, want) {
			t.Errorf("stored %+v, want %+v", got, want)
		}
	})

	t.Run("I7 wrong token", func(t *testing.T) {
		if code := itPost(t, base, token+"x", []itSample{{"it_auth", map[string]string{"run": run}, 1, now}}); code != 401 {
			t.Errorf("status %d, want 401", code)
		}
		if got := itQuery(t, prom, fmt.Sprintf(`it_auth{run=%q}[1h]`, run), now+1000); len(got) != 0 {
			t.Errorf("stored %+v, want nothing", got)
		}
	})
}
