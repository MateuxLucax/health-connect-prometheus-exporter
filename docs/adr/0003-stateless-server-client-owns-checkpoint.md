# 3. Stateless server; the client owns the checkpoint

Status: Accepted

## Context

If Prometheus is down, samples must not be lost. One option is a queue or
database in the server that buffers samples until Prometheus is back. The
data already lives durably in Health Connect on the phone, and the app reads
it through the Changes API with a checkpoint.

## Decision

The server keeps no state: no queue, no disk, no database. It answers `204`
only after every sample was written or rejected for good, and `502`
otherwise. The app advances its Health Connect changes token only after a
2xx and re-sends the same data after a failure.

## Consequences

- Nothing to persist, back up or migrate; the container can be restarted at
  any time.
- Delivery is at least once: after a `502`, batches that were already
  written are sent again. Prometheus keeps the stored value for an existing
  timestamp, so this is harmless.
- While Prometheus is down, data waits on the phone, not on the server.
- Requests are independent, so concurrent requests need no coordination.
