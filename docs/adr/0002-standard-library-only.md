# 2. Standard library only

Status: Accepted

## Context

The server is small and handles personal health data. Remote write needs
protobuf and snappy, which normally means `prometheus/prompb`,
`gogo/protobuf` and `golang/snappy` plus their transitive dependencies, all
of which must be tracked and updated.

## Decision

Use only the Go standard library (`go.mod` has no `require`). The
`WriteRequest` protobuf is hand-encoded in `encodeWriteRequest` (four message
types, five fields). Snappy is encoded with literal elements only
(`snappyEncode`): valid block-format snappy that does not compress.

## Consequences

- Zero third-party code: no dependency updates, small static binary on a
  distroless image.
- Payloads are slightly larger than with real compression. Requests go over
  a local network to Prometheus, so this does not matter.
- Correctness of the hand-written encoders is checked by `spec_test.go`,
  whose fake Prometheus decodes requests with an independent snappy and
  protobuf decoder written from the public format descriptions, and by the
  integration test against a real Prometheus in CI.
