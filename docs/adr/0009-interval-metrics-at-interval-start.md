# 9. Store interval metrics as the amount at the interval start

Status: Accepted

## Context

Steps, distance and calories come from Health Connect as amounts over an
interval (start and end time). Prometheus samples have one timestamp.
Options: a running counter (needs state and breaks with late or edited
records), aggregation on the phone (loses detail and needs to know when an
interval is complete), or one sample per record.

## Decision

Each interval record becomes one sample whose value is the amount in that
interval and whose timestamp is the interval start. Nothing is aggregated on
the phone or the server. Totals are computed in PromQL with
`sum_over_time`, e.g. `sum_over_time(health_steps[1d] offset 1ms)` for a
calendar day.

## Consequences

- No state or aggregation logic anywhere; late and edited records just add
  or replace samples.
- These are not counters: `rate()` and `increase()` do not apply.
- Prometheus 3 range selectors are left-open, so day totals need
  `offset 1ms` (explained in the README).
