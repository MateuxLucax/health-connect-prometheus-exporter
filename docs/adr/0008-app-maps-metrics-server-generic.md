# 8. The app maps metrics; the server is generic

Status: Accepted

## Context

Someone has to turn Health Connect records into Prometheus series: choose
metric names, units, labels and which timestamp to use. Doing it in the
server would tie it to Health Connect's data model and require a server
release for every new data type.

## Decision

The app maps records to samples (`Metrics.kt` in health-connect-bridge). The
server accepts any sample with a valid metric name, labels, value and
timestamp, and knows nothing about health data.

## Consequences

- New metrics need only an app update.
- The server stays small and could serve other clients.
- The server cannot enforce a schema; it only enforces Prometheus naming
  rules and size limits.
- The metric catalog is documented in both repos ([data.md](../data.md#metric-catalog)).
