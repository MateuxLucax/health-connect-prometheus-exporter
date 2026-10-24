# 7. Require an out-of-order window of 30 days in Prometheus

Status: Accepted

## Context

Samples arrive late and out of order: the watch syncs in batches, and the
app's first sync reads the last 7 days (30 days after an expired changes
token). By default Prometheus rejects a sample that is older than the newest
sample already stored for its series.

## Decision

Prometheus must set `storage.tsdb.out_of_order_time_window`. The repo's
`prometheus.yml` sets `30d`, matching the app's longest automatic re-read.
The example compose file also sets `--storage.tsdb.retention.time=10y`
because the default 15 days is too short for health history.

## Consequences

- Late batches and the 30-day re-read are accepted.
- Older samples (for example a long manual backfill) are rejected with `400
  too old sample` and dropped ([ADR 0005](0005-isolate-rejected-samples.md));
  the app tells the user about the 30-day window in its history import
  screen.
- The CI integration test runs Prometheus with this `prometheus.yml`.
