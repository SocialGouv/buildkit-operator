# 0004 — One shared SNI gateway for off-cluster CI (not one LB per daemon)

**Status:** Accepted · **Date:** 2026-06-26 (backfilled)

## Context

In-cluster CI runners reach a daemon directly over its `ClusterIP` Service. **Off-cluster** runners
(GitHub-hosted, other clusters) cannot — they need a routable address per daemon. With one daemon per
project ([0001](0001-control-plane-over-vanilla-buildkit.md)) and projects growing without bound, the
naive "a public LoadBalancer per daemon" does not scale (cost, IP exhaustion, provisioning latency)
and would force daemons off `ClusterIP`.

mTLS is non-negotiable: a build daemon authenticates clients by **client certificate**, end-to-end.

## Decision

Front **every** daemon with a **single shared SNI gateway** (`cmd/gateway`) behind **one**
LoadBalancer. The gateway terminates **no TLS**: it peeks the TLS ClientHello's **SNI**
(`<daemon>.<gateway-host>`), maps `<daemon>` to that project's in-cluster `ClusterIP` Service, and
pipes the still-encrypted bytes through. mTLS stays **end-to-end** to the daemon. buildd returns a
**deterministic** endpoint `tcp://<daemon>.<gateway-host>:1234` from `/route` — computed straight from
the key, no polling for an LB IP. Daemons stay `ClusterIP`.

## Alternatives considered

- **One public LB per daemon.** Rejected: doesn't scale with project count; slow to provision; forces
  daemons off ClusterIP (bigger attack surface).
- **A TLS-terminating L7 reverse proxy / Ingress.** Rejected: it would break end-to-end mTLS (the proxy
  sees plaintext and must hold client trust), making the proxy a high-value MITM point. SNI peeking
  keeps the gateway a dumb, trustless pipe.
- **An L4 LB with per-daemon listeners/ports.** Rejected: port management per project is operationally
  ugly and still doesn't give a stable per-daemon hostname.

## Consequences

- ✅ Fixed external surface (one LB) regardless of project count; daemons stay `ClusterIP`.
- ✅ End-to-end mTLS preserved — the gateway never sees plaintext and rejects any SNI outside its
  domain or that isn't a `buildkitd-` daemon name (defense in depth).
- ✅ Deterministic endpoint = no service-discovery round-trip; the client can pre-create its buildx
  builder from the key alone.
- ⚠️ The daemon certificate's SAN **must** cover `*.<gateway-host>`, and a wildcard DNS
  `*.<gateway-host>` must point at the gateway LB. A missing SAN is a confusing TLS failure — buildd
  now logs a **boot-time warning** when `gateway.host` is set but the cert doesn't cover it (see
  [security review hardening](../../docs/operations.md#troubleshooting--common-failure-modes)).
- ⚠️ The gateway assumes the ClientHello fits one TLS record (true for all real clients). A pathological
  fragmented ClientHello is rejected early.
- 🔁 Off by default (`gateway.host` unset): with in-cluster runners there is **no** public daemon
  surface at all.
