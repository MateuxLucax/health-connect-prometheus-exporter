# 6. Drop samples more than 1 hour in the future

Status: Accepted

## Context

A device with a wrong clock can produce samples far in the future. Prometheus
would accept them, which moves the TSDB head time forward. With the head in
the future, normal samples fall outside the out-of-order window and are
rejected as "too old", so one bad sample could break all later syncs.
Rejecting the whole request with `400` would block the app's sync on data it
cannot fix.

## Decision

After validation, `dropFuture` removes samples whose timestamp is more than
`maxFuture` (1 hour) after the server's clock, logs how many, and writes the
rest. The request still returns `204`.

## Consequences

- A bad clock cannot poison the TSDB head.
- Those samples are lost silently for the client; the server log records
  the count.
- An hour of slack tolerates normal clock skew between phone and server.
