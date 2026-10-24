# 10. Require a 32-character token; leave TLS to a reverse proxy

Status: Accepted

## Context

The endpoint accepts health data and writes it to Prometheus, so it needs
authentication. It typically runs on a home server reached over the LAN,
sometimes from the internet. Built-in TLS would mean certificate handling in
the server.

## Decision

- A single shared bearer token, `AUTH_TOKEN`, compared in constant time. The
  server refuses to start if it is shorter than 32 characters (the README
  suggests `openssl rand -hex 32`).
- The server speaks plain HTTP only. HTTPS, when needed, is terminated by a
  reverse proxy in front of it.

## Consequences

- Simple setup on a trusted LAN; the app allows cleartext HTTP for this.
- Without a proxy, the token and the health data travel in cleartext. The
  README requires HTTPS whenever the phone does not reach the server over a
  trusted LAN.
- One token for all clients; rotating it means changing the env var and the
  app setting.
