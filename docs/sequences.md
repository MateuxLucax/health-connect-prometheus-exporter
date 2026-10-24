# Sequences

- [Normal request](#normal-request)
- [Partial rejection](#partial-rejection)
- [Upstream misconfigured or unreachable](#upstream-misconfigured-or-unreachable)
- [Authentication failure](#authentication-failure)
- [End to end: app, exporter, Prometheus](#end-to-end-app-exporter-prometheus)

## Normal request

```mermaid
sequenceDiagram
    autonumber
    participant App as Bridge app
    participant H as Handler (main.go)
    participant W as remoteWriter (remotewrite.go)
    participant P as Prometheus

    App->>H: POST /api/v1/samples (Bearer token, JSON, max 8 MiB)
    H->>H: constant-time compare of Authorization header
    H->>H: decodeSamples: JSON, names, labels, value, timestamp_ms > 0
    alt invalid body or sample
        H-->>App: 400 + short reason
    end
    H->>H: dropFuture: drop samples more than 1h ahead (logged)
    H->>W: write(groupSeries(samples))
    Note over W: group by metric + labels (empty values dropped),<br/>sort points by time, last sample wins per timestamp,<br/>split into batches of at most 10,000 points
    loop each batch
        W->>W: encodeWriteRequest + snappyEncode
        W->>P: POST REMOTE_WRITE_URL (snappy protobuf)
        P-->>W: 204
    end
    W-->>H: nil
    H-->>App: 204 No Content
```

An empty `samples` list produces no upstream request and returns `204`.

## Partial rejection

Prometheus discards a whole remote write request when one sample in it is
rejected (`400`). The exporter isolates the offending samples, resends the
rest and still answers `204`. Too-old samples are found with a binary search
on the timestamp cutoff; other `400`s are isolated by halving.

```mermaid
sequenceDiagram
    autonumber
    participant App as Bridge app
    participant H as Handler
    participant W as remoteWriter
    participant P as Prometheus

    App->>H: POST /api/v1/samples
    H->>W: write(series)
    W->>P: send(batch)
    P-->>W: 400 "too old sample" / "out of bounds"
    Note over W: findCutoff: sort single points by timestamp,<br/>binary search with one-point requests
    loop O(log n) probes
        W->>P: send(one point at mid)
        alt rejected as too old
            P-->>W: 400 too old
            Note over W: lo = mid + 1
        else accepted or other 400
            P-->>W: 204 or 400
            Note over W: hi = mid
        end
    end
    W->>W: dropBefore(cutoff): count dropped points
    W->>P: send(kept points) via sendIsolating
    P-->>W: 204
    W-->>H: rejectedError{dropped}
    Note over H: log "dropped N of M samples"
    H-->>App: 204 No Content
```

For a `400` that is not "too old" (for example "duplicate sample for
timestamp"), or when no point is too old on its own:

```mermaid
sequenceDiagram
    autonumber
    participant W as remoteWriter
    participant P as Prometheus

    W->>P: send(batch of n points)
    P-->>W: 400 duplicate sample for timestamp
    Note over W: split into halves of ceil(n/2) points
    W->>P: send(first half)
    P-->>W: 204
    W->>P: send(second half)
    P-->>W: 400
    Note over W: recurse until a 1-point batch is rejected:<br/>that point is dropped and counted
    W->>P: send(remaining sub-batches)
    P-->>W: 204
```

If any probe or resend gets a non-`400` error, the whole request fails with
`502` (next section).

## Upstream misconfigured or unreachable

Only `400` from Prometheus means "these samples are rejected for good".
Everything else fails the request so the app keeps its checkpoint and
retries.

```mermaid
sequenceDiagram
    autonumber
    participant App as Bridge app
    participant H as Handler
    participant W as remoteWriter
    participant P as Prometheus

    App->>H: POST /api/v1/samples
    H->>W: write(series)
    W->>P: send(batch)
    alt connection refused / DNS / 30 s timeout
        P--xW: network error
    else 401, 403, 404, 405, 413, 422, 429, 5xx
        P-->>W: non-2xx, non-400
    end
    W-->>H: plain error
    H->>H: log "remote write failed"
    H-->>App: 502 remote write failed
    Note over App: checkpoint unchanged,<br/>WorkManager retries later
```

Batches already written before the failure stay in Prometheus. The retry
resends them; Prometheus keeps the stored value for an existing timestamp
(a different value is a `400`, which is dropped), so retries are safe.

## Authentication failure

```mermaid
sequenceDiagram
    autonumber
    participant App as Bridge app
    participant H as Handler
    participant P as Prometheus

    App->>H: POST /api/v1/samples (missing or wrong Bearer token)
    H->>H: ConstantTimeCompare fails
    H-->>App: 401 unauthorized
    Note over H,P: body is not read, nothing is sent upstream
    Note over App: sync fails, error shown in the app,<br/>checkpoint unchanged
```

## End to end: app, exporter, Prometheus

The same diagram appears in the
[bridge docs](https://github.com/MateuxLucax/health-connect-bridge/blob/main/docs/sequences.md#end-to-end-app-exporter-prometheus).

```mermaid
sequenceDiagram
    autonumber
    participant WM as WorkManager
    participant S as HealthSync (app)
    participant HC as Health Connect
    participant SP as SharedPreferences
    participant E as Exporter
    participant P as Prometheus

    WM->>S: periodic job (every 15 min, network required)
    S->>SP: read changes token + granted types key
    S->>HC: getChanges(token)
    HC-->>S: upserted records, nextChangesToken, hasMore
    S->>S: map records to samples (metric, labels, value, timestamp_ms)
    loop chunks of at most 5,000 samples
        S->>E: POST /api/v1/samples (Bearer token)
        E->>E: auth, validate, drop future, group, batch
        E->>P: remote write (protobuf + snappy)
        alt Prometheus 2xx or 400 (rejected samples dropped)
            P-->>E: 204 / 400
            E-->>S: 204
        else unreachable or other status
            P-->>E: error
            E-->>S: 502
            S-->>WM: HttpStatusException(502), SyncWorker returns Result.retry()
        end
    end
    S->>SP: save nextChangesToken (only after every chunk got 2xx)
    S->>SP: last values, last success time
    Note over S,HC: repeat getChanges while hasMore
```
