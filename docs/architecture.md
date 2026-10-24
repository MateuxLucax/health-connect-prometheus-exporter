# Architecture

## System context

Health data travels from the watch to Grafana through two projects:
[health-connect-bridge](https://github.com/MateuxLucax/health-connect-bridge)
(Android app) and this exporter (Go server).

```mermaid
flowchart LR
    watch["Galaxy Watch"] -->|Bluetooth sync| shealth["Samsung Health"]
    shealth -->|writes records| hc["Health Connect<br/>(on-device store)"]
    hc -->|"Changes API / readRecords"| app["health-connect-bridge<br/>(Android app)"]
    app -->|"POST /api/v1/samples<br/>JSON + Bearer token"| exp["health-connect-prometheus-exporter<br/>(this repo)"]
    exp -->|"remote write 1.0<br/>protobuf + snappy"| prom[("Prometheus TSDB")]
    graf["Grafana"] -->|PromQL| prom
```

- The watch syncs to the phone late and in batches, so records reach Health
  Connect minutes or hours after they were measured.
- The app maps Health Connect records to Prometheus samples (metric name,
  labels, value, original timestamp) and owns the sync checkpoint.
- The exporter validates samples and forwards them with remote write, keeping
  the original timestamps. It holds no state.
- Prometheus must accept out-of-order samples
  (`out_of_order_time_window`, 30d in the example config).

## Components

One binary. `package main` at the repository root reads the configuration and
wires two packages under `internal/` together
([ADR 0012](adr/0012-code-structure.md)).

```mermaid
flowchart TB
    subgraph exporter["health-connect-prometheus-exporter (single binary)"]
        direction TB
        subgraph main["package main (main.go)"]
            cfg["main()<br/>env config: AUTH_TOKEN, REMOTE_WRITE_URL, LISTEN_ADDR<br/>http.Server timeouts"]
        end
        subgraph ingest["internal/ingest"]
            mux["NewHandler()<br/>ServeMux: GET /healthz, POST /api/v1/samples"]
            auth["Bearer token check<br/>(constant-time compare)"]
            dec["decodeSamples()<br/>JSON decode + validation, 8 MiB limit<br/>returns []remotewrite.Sample"]
            fut["dropFuture()<br/>drop samples more than 1h ahead"]
        end
        subgraph remotewrite["internal/remotewrite"]
            write["Client.Write()<br/>groupSeries: group by labels, sort, dedup timestamps"]
            rw["splitBatches (10,000 points)<br/>sendIsolating / findCutoff / dropBefore"]
            enc["encodeWriteRequest() + snappyEncode()<br/>hand-written protobuf, literal-only snappy"]
            send["Client.send()<br/>net/http client, 30 s timeout"]
        end
        cfg --> mux --> auth --> dec --> fut --> write --> rw
        rw --> enc --> send
    end
    send -->|"POST REMOTE_WRITE_URL"| prom[("Prometheus")]
```

### Import rule

```mermaid
flowchart LR
    main["package main (root)<br/>config + wiring"]
    ingest["internal/ingest<br/>HTTP handler, DTOs, validation"]
    rw["internal/remotewrite<br/>client, series, protobuf, snappy"]
    std["Go standard library"]
    main --> ingest
    main --> rw
    ingest --> rw
    ingest --> std
    rw --> std
```

- `main` → `ingest` → `remotewrite` → standard library. Never the other way.
- `remotewrite` knows nothing about the app's JSON contract. It exports only
  `Sample`, `Client`, `NewClient`, `(*Client).Write` and `RejectedError`.
- `ingest` owns the app contract ([data.md](data.md)). It exports only
  `NewHandler`, which takes the concrete `*remotewrite.Client` (no interface).
- `main` holds no logic beyond the configuration checks.

| File | Responsibility |
|------|----------------|
| `main.go` | Configuration from the environment, `http.Server` timeouts, wiring |
| `internal/ingest/handler.go` | Routing, auth, future-sample filter, status mapping, logging |
| `internal/ingest/request.go` | JSON request DTO (decoding only), validation and limits; `decodeSamples` returns `[]remotewrite.Sample` |
| `internal/remotewrite/client.go` | `Client`, `RejectedError`, batching, rejected-sample isolation, upstream HTTP |
| `internal/remotewrite/series.go` | `Sample` input, grouping into series, splitting into batches |
| `internal/remotewrite/protobuf.go` | Hand-written `WriteRequest` protobuf encoder |
| `internal/remotewrite/snappy.go` | Literal-only snappy block encoder |
| `internal/*/*_test.go` | Unit tests per package (white-box). Each package has its own copy of the few helpers it needs |
| `spec_test.go` | Black-box specification tests through `ingest.NewHandler` and `remotewrite.NewClient`: a fake Prometheus decodes requests with an independent snappy/protobuf decoder |
| `integration_test.go` | The compiled binary against a real Prometheus (build tag `integration`) |

## Deployment

```mermaid
flowchart LR
    subgraph phone["Android phone"]
        app["health-connect-bridge"]
    end
    subgraph server["Home server (Docker Compose)"]
        exp["exporter container<br/>distroless, nonroot<br/>port 8080 published"]
        prom["prometheus container<br/>prom/prometheus:v3.5.0<br/>port 9090 not published"]
        vol[("prometheus-data volume")]
        exp -->|"http://prometheus:9090/api/v1/write<br/>(compose network)"| prom
        prom --- vol
    end
    proxy["optional HTTPS reverse proxy"]
    app -->|"HTTP on trusted LAN"| exp
    app -.->|"HTTPS from outside"| proxy -.-> exp
    graf["Grafana"] -->|"PromQL (e.g. 127.0.0.1:9090)"| prom
```

- Image: `ghcr.io/mateuxlucax/health-connect-prometheus-exporter`, multi-arch
  (`linux/amd64`, `linux/arm64`), built by CI from the `Dockerfile` (static
  `CGO_ENABLED=0` binary on `distroless/static-debian12:nonroot`).
- `docker-compose.example.yml` runs the exporter and Prometheus. Prometheus
  starts with `--web.enable-remote-write-receiver` and
  `--storage.tsdb.retention.time=10y`, and mounts `prometheus.yml`
  (`out_of_order_time_window: 30d`).
- `GET /healthz` returns `200 ok` for container health checks.

## Trust boundaries

```mermaid
flowchart LR
    subgraph device["Phone (trusted, user's device)"]
        app["bridge app<br/>token in app-private SharedPreferences"]
    end
    subgraph net["Network (LAN or internet)"]
        wire["HTTP or HTTPS<br/>carries Bearer token + health data"]
    end
    subgraph host["Home server"]
        exp["exporter<br/>checks Authorization header"]
        prom["Prometheus<br/>no authentication"]
    end
    app --> wire --> exp
    exp -->|"cleartext HTTP on the compose network"| prom
```

| Boundary | Control |
|----------|---------|
| Phone to exporter | `Authorization: Bearer <AUTH_TOKEN>` checked with `crypto/subtle.ConstantTimeCompare` on every `POST /api/v1/samples` before the body is read. Wrong or missing token: `401`. `/healthz` is unauthenticated and returns no data. |
| Transport | The exporter speaks plain HTTP only; it has no TLS. The app allows cleartext (`usesCleartextTraffic="true"`) for LAN use. Outside a trusted LAN, put the exporter behind an HTTPS reverse proxy. |
| Token strength | The server refuses to start with an `AUTH_TOKEN` shorter than 32 characters. |
| Input | Body limited to 8 MiB; metric and label names validated; at most 16 labels of at most 1024 bytes each; label names starting with `__` rejected, so clients cannot set `__name__` or other reserved labels. |
| Exporter to Prometheus | Cleartext HTTP on the compose network. Prometheus has no authentication, so the example does not publish its port. Credentials in `REMOTE_WRITE_URL` are redacted in the startup log. |
