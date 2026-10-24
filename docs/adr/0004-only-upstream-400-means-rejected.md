# 4. Only an upstream 400 means "rejected samples"; everything else is 502

Status: Accepted

## Context

Prometheus answers a remote write with 2xx, with `400` when samples are
invalid for the TSDB (too old, out of bounds, duplicate timestamp with a
different value), or with other statuses for problems that are not about the
data: `404` when the receiver is disabled or the path is wrong, `401`/`403`
from a proxy, `413`, `429`, `5xx`. Treating every 4xx as "bad data" would
make the server drop samples, and the app advance its checkpoint, because of
a configuration mistake.

## Decision

`Client.send` (`internal/remotewrite`) maps only `400` to `RejectedError`.
Rejected samples are dropped and logged, and the app gets `204`. Any other non-2xx status, and any
network error or timeout, fails the request with `502`.

## Consequences

- A misconfigured or unavailable Prometheus never loses data: the app keeps
  its checkpoint and retries until it is fixed.
- Samples that Prometheus will never accept do not block the sync forever.
- `TestSpecUpstreamErrors` pins the mapping for 400, 401, 403, 404, 405, 413,
  422, 429, 500, 503 and an unreachable upstream.
