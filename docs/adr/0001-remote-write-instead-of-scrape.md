# 1. Use Prometheus remote write instead of a scrape exporter

Status: Accepted

## Context

Health data reaches the phone late and in batches: the watch syncs to Samsung
Health when it can, and records can be edited later. A classic exporter
exposes `/metrics` and Prometheus stamps each value with the scrape time.
That would put a heart rate measured at 03:00 at whatever time it was
scraped, and lose every value that changes between two scrapes.

## Decision

The server receives samples with their original timestamps and pushes them
to Prometheus with the remote write 1.0 protocol (`POST
REMOTE_WRITE_URL`, snappy-compressed protobuf). Prometheus runs with
`--web.enable-remote-write-receiver`.

## Consequences

- Every sample keeps its real timestamp, and historical data can be
  backfilled.
- Prometheus must accept out-of-order samples ([ADR 0007](0007-out-of-order-window.md)).
- Any remote write 1.0 receiver works, for example VictoriaMetrics'
  `/api/v1/write`.
- There is no `/metrics` endpoint; Prometheus does not scrape this server.
