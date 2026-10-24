# Architecture Decision Records

Short records of the decisions behind the exporter (Status, Context, Decision,
Consequences). The app's decisions are in the
[health-connect-bridge ADRs](https://github.com/MateuxLucax/health-connect-bridge/tree/main/docs/adr).

| # | Decision | Status |
|---|----------|--------|
| [0001](0001-remote-write-instead-of-scrape.md) | Use Prometheus remote write instead of a scrape exporter | Accepted |
| [0002](0002-standard-library-only.md) | Standard library only | Accepted |
| [0003](0003-stateless-server-client-owns-checkpoint.md) | Stateless server; the client owns the checkpoint | Accepted |
| [0004](0004-only-upstream-400-means-rejected.md) | Only an upstream 400 means "rejected samples"; everything else is 502 | Accepted |
| [0005](0005-isolate-rejected-samples.md) | Isolate rejected samples with a timestamp binary search and halving | Accepted |
| [0006](0006-drop-future-samples.md) | Drop samples more than 1 hour in the future | Accepted |
| [0007](0007-out-of-order-window.md) | Require an out-of-order window of 30 days in Prometheus | Accepted |
| [0008](0008-app-maps-metrics-server-generic.md) | The app maps metrics; the server is generic | Accepted |
| [0009](0009-interval-metrics-at-interval-start.md) | Store interval metrics as the amount at the interval start | Accepted |
| [0010](0010-token-and-no-in-process-tls.md) | Require a 32-character token; leave TLS to a reverse proxy | Accepted |
| [0011](0011-automatic-releases.md) | Release automatically on every green push to main | Accepted |
| [0012](0012-code-structure.md) | Code structure: `main` at the root, logic in `internal/`, no interfaces | Accepted |
