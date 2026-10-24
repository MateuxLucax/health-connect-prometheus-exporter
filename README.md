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
advances its sync checkpoint after a 2xx response, so retrying is up to the app.

## API

### `POST /api/v1/samples`

Headers: `Authorization: Bearer <AUTH_TOKEN>`, `Content-Type: application/json`.
Body (max 32 MiB):

```json
{"samples":[
  {"metric":"health_heart_rate_bpm","labels":{"source":"com.sec.android.app.shealth"},"value":72,"timestamp_ms":1792839600000}
]}
```

- `metric` must match `^[a-zA-Z_:][a-zA-Z0-9_:]*$`.
- Label names must match `^[a-zA-Z_][a-zA-Z0-9_]*$` and must not start with `__`.
  Labels with an empty value are ignored (Prometheus treats empty as absent).
- `value` is required. `timestamp_ms` is Unix time in milliseconds and must be > 0.
- If one series has several samples with the same timestamp, the last one wins.

| Status | Meaning |
|--------|---------|
| 204 | Accepted (also for an empty `samples` list). Also returned when Prometheus rejects samples for good (4xx other than 429, such as "too old sample" or "duplicate sample for timestamp"): they are logged and dropped, because retrying would never help. The other samples in the request are still written. |
| 400 | Malformed request; the body has a short reason. |
| 401 | Missing or wrong token. |
| 502 | Prometheus unreachable, 5xx or 429. Retry later. |

### `GET /healthz`

Returns `200 ok`, for container health checks.

## Configuration

| Variable | Default | |
|----------|---------|---|
| `AUTH_TOKEN` | (required) | Bearer token the app must send. The server refuses to start without it. |
| `REMOTE_WRITE_URL` | (required) | e.g. `http://prometheus:9090/api/v1/write`. VictoriaMetrics' `/api/v1/write` works too. |
| `LISTEN_ADDR` | `:8080` | |

Put the server behind HTTPS (a reverse proxy) if it is reachable from outside your network.

## Prometheus requirements

- Start Prometheus with `--web.enable-remote-write-receiver`.
- Set `storage.tsdb.out_of_order_time_window` (for example `30d`) in
  `prometheus.yml`. Data arrives late and out of order. Samples older than
  the window are rejected by Prometheus and dropped by this server.
- Prometheus keeps data for 15 days by default. For health history set
  something like `--storage.tsdb.retention.time=10y`.

See [`docker-compose.example.yml`](docker-compose.example.yml) and
[`prometheus.yml`](prometheus.yml):

```sh
cp docker-compose.example.yml docker-compose.yml   # then change AUTH_TOKEN
docker compose up -d
```

## Querying

Interval metrics such as `health_steps`, `health_active_calories_kcal`,
`health_total_calories_kcal` and `health_distance_meters` are stored as
"amount in the interval", timestamped at the interval start. Sum them over a
range to get totals:

```promql
# Steps per day
sum_over_time(health_steps[1d])

# Distance walked in the last 7 days, in km
sum_over_time(health_distance_meters[7d]) / 1000

# Average and maximum heart rate over the last day
avg_over_time(health_heart_rate_bpm[1d])
max_over_time(health_heart_rate_bpm[1d])
```

## Development

```sh
go vet ./... && go test ./...
AUTH_TOKEN=secret REMOTE_WRITE_URL=http://localhost:9090/api/v1/write go run .
```

### Integration tests

`integration_test.go` runs the compiled server against a real Prometheus. Start
Prometheus with `--web.enable-remote-write-receiver` and the
[`prometheus.yml`](prometheus.yml) from this repo (it sets the 30d
out-of-order window), then:

```sh
PROMETHEUS_URL=http://localhost:9090 go test -tags integration -count=1 ./...
```

Without `PROMETHEUS_URL` the integration tests are skipped. Each run uses a
unique `run` label, so it can reuse the same Prometheus.

## License

[MIT](LICENSE)
