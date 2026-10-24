# 12. Code structure: `main` at the root, logic in `internal/`, no interfaces

Status: Accepted

## Context

All code was in two files of `package main`: `main.go` (configuration, HTTP
handler, JSON DTOs, validation) and `remotewrite.go` (grouping, batching,
rejected-sample isolation, protobuf, snappy, upstream HTTP). Nothing kept the
HTTP API from reaching into the remote write internals, and the tests shared
unexported helpers across files.

The repository builds one binary. Several things build it with `go build .`
from the repository root: the health-connect-bridge CI (its end-to-end job
checks out this repository's `main`), `TestSpecStartup`, `TestIntegration`,
the `Dockerfile` and the README's `go run .`.

## Decision

Follow the official "command with supporting packages" layout from
[Organizing a Go module](https://go.dev/doc/modules/layout):

```
main.go                package main: env config + wiring
internal/ingest/       HTTP API the app talks to: NewHandler, JSON DTO, validation
internal/remotewrite/  Prometheus remote write 1.0 client: Sample, Client, NewClient,
                       Write, RejectedError; series, protobuf, snappy
```

- Imports go one way: `main` → `ingest` → `remotewrite` → standard library.
  `remotewrite` knows nothing about the app's JSON contract.
- Small exported surface. `ingest` exports only `NewHandler`; `remotewrite`
  exports only `Sample`, `Client`, `NewClient`, `(*Client).Write` and
  `RejectedError`. `Write` groups samples itself, so `series`, `label` and
  `point` stay unexported.
- The JSON DTO (`Value *float64`, to tell a missing value from `0`) is used
  only for decoding. `decodeSamples` validates each sample and builds the
  `remotewrite.Sample` (`Value float64`) slice directly, so the handler works
  on that slice without another copy.
- `ingest.NewHandler` takes the concrete `*remotewrite.Client`. There is no
  interface between the packages: Go interfaces belong to the consumer and
  only once a second implementation exists
  ([Code Review Comments](https://go.dev/wiki/CodeReviewComments#interfaces)).
  The tests run the real client against `httptest` servers.
- Unit tests are white-box, one `_test.go` per source file, so encoder
  functions are not exported for tests. `spec_test.go` stays at the root and is
  black-box: it uses only `ingest.NewHandler`, `remotewrite.NewClient` and the
  compiled binary. Each test package has its own copy of the few helpers it
  needs (`testClient`, `specLabels`, and in the root and `remotewrite` the
  independent snappy/protobuf decoder). `ingest` tests do not decode the wire
  format: they test `dropFuture` and validation directly, and the root
  `spec_test.go` covers what reaches Prometheus.

Not used, and why:

- `cmd/<name>/`: one binary, and `main` at the root is an official layout.
  Moving it would break every `go build .` above. Revisit if a second binary
  appears.
- `pkg/`: not part of the official guidance; nothing here is meant for other
  modules to import.
- Packages named `models`, `types`, `api`, `handlers`, `utils`, `common` or
  `config` ([Package names](https://go.dev/blog/package-names)). The
  configuration is about 20 lines of `os.Getenv` in `main.go`.
- An interface per struct, service or repository layers, a shared
  `internal/testutil` package, and third-party routers or DI libraries
  ([ADR 0002](0002-standard-library-only.md)).

## Consequences

- The compiler enforces the boundaries: nothing outside the module can import
  `internal/...`, and `remotewrite` cannot import `ingest` (that would be an
  import cycle, a build error).
- `go build .`, `go run .` and the bridge's end-to-end build keep working
  unchanged. The `Dockerfile` copies `internal/` as well as the root `*.go`.
- CI's `go vet ./...`, `go vet -tags integration ./...` and
  `go test -race ./...` walk the new packages automatically.
- The independent test decoder exists in two copies (root and
  `remotewrite`). It is test-only code that rarely changes; a package that
  exists only for tests would cost more.
