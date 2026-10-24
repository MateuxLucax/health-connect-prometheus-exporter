# 5. Isolate rejected samples with a timestamp binary search and halving

Status: Accepted

## Context

Prometheus discards an entire remote write request when any one sample in it
is rejected. Health history often contains samples older than the
out-of-order window (a backfill of 90 days with a 30-day window), mixed with
valid ones. Dropping the whole batch would lose valid data; sending samples
one by one would need thousands of requests.

## Decision

`sendIsolating` handles a `400` like this:

- If the message says the sample is too old (`too old sample` or `out of
  bounds`), `findCutoff` sorts the batch's points by timestamp and binary
  searches, with one-point requests, for the oldest timestamp Prometheus
  accepts. Points before the cutoff are dropped (`dropBefore`) and the rest
  resent: O(log n) extra requests.
- For any other `400`, or if no point is too old on its own, the batch is
  split in two halves and each half is sent recursively, until a rejected
  batch of one point is dropped.

## Consequences

- Valid samples next to invalid ones are always written.
- All-too-old batches cost a logarithmic number of requests
  (`TestWriteAllTooOldIsLogarithmic`).
- Depends on Prometheus' error message text for the fast path; if the text
  changes, halving still finds the bad samples, only slower.
