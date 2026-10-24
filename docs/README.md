# Documentation

Architecture documentation for health-connect-prometheus-exporter. The Android
side is documented in the
[health-connect-bridge docs](https://github.com/MateuxLucax/health-connect-bridge/tree/main/docs).

| Document | Contents |
|----------|----------|
| [architecture.md](architecture.md) | System context, components, deployment, trust boundaries |
| [sequences.md](sequences.md) | Request handling, partial rejection, upstream errors, auth failure, end-to-end sync |
| [data.md](data.md) | HTTP API contract, metric catalog, remote write protobuf and snappy layout |
| [adr/](adr/README.md) | Architecture Decision Records |

All diagrams are [Mermaid](https://mermaid.js.org/) and render on GitHub.
