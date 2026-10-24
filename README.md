# health-connect-prometheus-exporter

A small server that receives health samples from the
[health-connect-bridge](https://github.com/MateuxLucax/health-connect-bridge)
Android app and writes them to Prometheus with the
[remote write](https://prometheus.io/docs/specs/prw/remote_write_spec/) 1.0
protocol, keeping each sample's original timestamp.

```
Galaxy Watch → Samsung Health → Health Connect → health-connect-bridge app → this server → Prometheus
```

Health data reaches the phone late and in batches, so a scrape-based exporter
would stamp it with the wrong time. Remote write keeps the real timestamps.

The server is stateless and uses only the Go standard library. The app only
advances its sync checkpoint after a 2xx response and retries after a 502, so
retrying is up to the app.

## API

### `POST /api/v1/samples`

Headers: `Authorization: Bearer <AUTH_TOKEN>`, `Content-Type: application/json`.
Body (max 8 MiB):

```json
{"samples":[
  {"metric":"health_heart_rate_bpm","labels":{"source":"com.sec.android.app.shealth"},"value":72,"timestamp_ms":1792839600000}
]}
```

- `metric` must match `^[a-zA-Z_:][a-zA-Z0-9_:]*$`.
- At most 16 labels per sample. Label names must match
  `^[a-zA-Z_][a-zA-Z0-9_]*$` and must not start with `__`. Label values are
  at most 1024 bytes. Labels with an empty value are ignored (Prometheus
  treats empty as absent).
- `value` is required. `timestamp_ms` is Unix time in milliseconds and must be > 0.
- Samples more than 1 hour in the future (a wrong clock on a device) are
  logged and dropped; the rest of the request is still written. Such a sample
  would move Prometheus' head time forward and make normal samples "too old".
- If one series has several samples with the same timestamp, the last one wins.

| Status | Meaning |
|--------|---------|
| 204 | Accepted (also for an empty `samples` list). Also returned when Prometheus answers **400** for some samples (such as "too old sample" or "duplicate sample for timestamp"): those are logged and dropped, because retrying would never help, and the other samples in the request are still written. |
| 400 | Malformed request or a sample over the limits above; the body has a short reason. |
| 401 | Missing or wrong token. |
| 502 | Prometheus unreachable or answered anything other than 2xx or 400: 5xx, 429, or a misconfiguration such as 404 (remote write receiver not enabled, wrong `REMOTE_WRITE_URL` path), 401/403 from a proxy or 413. Nothing is dropped; the app keeps its checkpoint and retries. |

Prometheus rejects a whole remote write request if one sample in it is bad,
so the server finds the offending samples and resends the rest. Samples
rejected as too old are found with a binary search on the timestamp (a few
requests even when thousands are too old); for other rejections the batch is
halved until the bad samples are isolated.

### `GET /healthz`

Returns `200 ok`, for container health checks.

## Configuration

| Variable | Default | |
|----------|---------|---|
| `AUTH_TOKEN` | (required) | Bearer token the app must send, at least 32 characters (`openssl rand -hex 32`). The server refuses to start without it. |
| `REMOTE_WRITE_URL` | (required) | e.g. `http://prometheus:9090/api/v1/write`. VictoriaMetrics' `/api/v1/write` works too. Credentials in the URL are redacted in the log. |
| `LISTEN_ADDR` | `:8080` | |

The token and your health data travel in plain HTTP unless you put the
server behind HTTPS (a reverse proxy). Use HTTPS whenever the phone does not
reach the server over a trusted LAN, in particular if it is reachable from
the internet.

## Prometheus requirements

- Start Prometheus with `--web.enable-remote-write-receiver`.
- Set `storage.tsdb.out_of_order_time_window` (for example `30d`) in
  `prometheus.yml`. Data arrives late and out of order. Samples older than
  the window are rejected by Prometheus (400 "too old sample") and dropped by
  this server.
- Prometheus keeps data for 15 days by default. For health history set
  something like `--storage.tsdb.retention.time=10y`.

See [`docker-compose.example.yml`](docker-compose.example.yml) and
[`prometheus.yml`](prometheus.yml):

```sh
cp docker-compose.example.yml docker-compose.yml
echo "AUTH_TOKEN=$(openssl rand -hex 32)" > .env    # use the same token in the app
docker compose up -d
```

Prometheus has no authentication, so the example does not publish its port;
the exporter reaches it on the compose network. To open it from Grafana or a
browser on the same machine, publish it on localhost only:
`127.0.0.1:9090:9090`.

## Docker image

Multi-arch images (`linux/amd64`, `linux/arm64`) are published to GHCR by CI:

```sh
docker pull ghcr.io/mateuxlucax/health-connect-prometheus-exporter:latest  # latest release
docker pull ghcr.io/mateuxlucax/health-connect-prometheus-exporter:main    # tip of main
```

Releases (`vX.Y.Z` tags) are also tagged `X.Y.Z` and `X.Y`. GHCR creates the
package as private; the repository owner makes it public once in the
package settings.

## Querying

Interval metrics such as `health_steps`, `health_active_calories_kcal`,
`health_total_calories_kcal` and `health_distance_meters` are stored as
"amount in the interval", timestamped at the interval start. Sum them over a
range to get totals:

```promql
# Steps of the day that ends at the evaluation time (see below)
sum_over_time(health_steps[1d] offset 1ms)

# Distance walked in the last 7 days, in km
sum_over_time(health_distance_meters[7d]) / 1000

# Average and maximum heart rate over the last day
avg_over_time(health_heart_rate_bpm[1d])
max_over_time(health_heart_rate_bpm[1d])
```

Range selectors are left-open in Prometheus 3: `[1d]` evaluated at `t` covers
`(t-1d, t]`. Evaluated exactly at midnight, `sum_over_time(health_steps[1d])`
therefore leaves out that day's 00:00 sample and includes the next day's
00:00 sample. Either evaluate it 1 ms before midnight, or evaluate at midnight
with `offset 1ms` as above, which covers exactly `[00:00, 24:00)` of the day
that just ended. For a daily bar chart in Grafana, use the `offset 1ms` query
with a 1d interval (Grafana aligns daily steps to UTC midnight, so each value
is the UTC day before its timestamp).

## Development

```sh
go vet ./... && go test ./...
AUTH_TOKEN=$(openssl rand -hex 32) REMOTE_WRITE_URL=http://localhost:9090/api/v1/write go run .
```

### Integration tests

`integration_test.go` runs the compiled server against a real Prometheus. Start
Prometheus with `--web.enable-remote-write-receiver` and the
[`prometheus.yml`](prometheus.yml) from this repo (it sets the 30d
out-of-order window), then:

```sh
PROMETHEUS_URL=http://localhost:9090 go test -tags integration -count=1 -run TestIntegration ./...
```

Without `PROMETHEUS_URL` the integration tests are skipped. Each run uses a
unique `run` label, so it can reuse the same Prometheus.

## License

[MIT](LICENSE)
