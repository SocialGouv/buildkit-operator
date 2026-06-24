# Architecture

buildcat is a **control plane** over stock `buildkitd`. It owns two things — **routing** (send
builds that should share a cache to the same daemon) and **lifecycle** (keep that daemon warm,
scale it to zero, snapshot it, clone it). It deliberately owns **nothing** at the storage layer:
the daemon's content store, snapshots, and bbolt metadata are vanilla.

```
   client (CI runner)                         Kubernetes namespace `buildcat`
  ┌──────────────┐   POST /route          ┌──────────────────────────────────────────────┐
  │  build.sh /  │ ─────────────────────► │  buildd (Deployment, replicas: 2, HA)         │
  │  build CLI   │ ◄──── endpoint ─────── │   • controller-runtime manager (leader only)  │
  └──────┬───────┘                        │   • HTTP API /route /prewarm (all replicas)   │
         │ buildx remote (TCP + mTLS)     │   • Prometheus /metrics                        │
         ▼                                └───────────────────┬──────────────────────────┘
  ┌─────────────────────────┐  reconciles                     │ creates / scales / snapshots
  │ StatefulSet-of-1         │ ◄───────────────────────────────┘
  │  buildkitd (vanilla)     │   one per (project, arch)
  │  + companion (sidecar)   │   Service :1234 (TCP + mTLS)  — ClusterIP, or LoadBalancer in gateway mode
  │  + Cinder gen2 PVC (warm)│
  └─────────────────────────┘
```

## The routing key — the one invariant that matters

All builds that must share a cache **must resolve to the same key** ⇒ the same `StatefulSet` ⇒ the
same daemon. This is the heart of the design and lives in [`internal/router`](../internal/router)
as a **pure function**, shared verbatim by the CLI and the control plane so they can never disagree.

| Function | Formula | Purpose |
|---|---|---|
| `ProjectKey(repo, target, arch)` | `"p" + sha256(normRepo \x00 normTarget \x00 normArch)[:16]` | the canonical cache identity |
| `ForkKey(canonicalKey)` | `"fork" + key` | an **ephemeral, isolated** daemon for untrusted/fork PRs |
| `CloneKey(canonicalKey, i)` | `"c" + i + key` | the i-th **CoW clone** for fan-out (M5) |
| `CachePVCName(key)` | `"cache-buildkitd-" + key + "-0"` | the StatefulSet's `volumeClaimTemplate` PVC |
| `EndpointHost(host, port)` | `tcp://host:port` | used by the public gateway to return an LB address |

Design points proven out in this session:

- **The key is coarse on purpose** — no branch, no commit, no build context. Two concurrent builds
  and a build an hour later all converge to the same daemon, so they share layers and cache mounts.
  A finer key (e.g. per-branch) would fragment the cache and defeat the whole point.
- **`target` is part of the key** because different Dockerfile target stages have genuinely
  different caches; folding them together would thrash.
- **`arch` is part of the key** because a daemon is single-arch (it builds natively; cross-arch is a
  separate daemon, not QEMU-in-one).

Example: `SocialGouv/buildcat-example` + `amd64` → `pa081c22c974da132` (the daemon name you see
running on the cluster).

## The reconcile loop

The `BuildProject` reconciler ([`internal/controller`](../internal/controller)) is a standard
controller-runtime loop. Per object it converges:

1. **StatefulSet-of-1 + Service + PVC** — rendered by [`internal/builder`](../internal/builder) with
   the rootless security profile and the gen2 `volumeClaimTemplate`. The Service is the stable mTLS
   endpoint `:1234`.
2. **`desiredReplicas`** — tier- and idle-aware **scale-to-zero** (M2). When a `warm`/`cold` project
   goes idle past `idleTimeoutSec`, it scales the StatefulSet to 0 **but keeps the PVC** — so waking
   up is an attach, not a restore. `hot` never scales to zero.
3. **`maybeSnapshot`** — periodic **in-use** `VolumeSnapshot` (M3) on the `snapshotEverySec`
   cadence, using OVH's in-use snapclass so the daemon does **not** need to scale to zero to be
   snapshotted. Old snapshots are pruned to `--keep-snapshots`.
4. **`reconcileFanout`** — when `spec.fanout > 0` (M5), materializes N **CoW clone** daemons
   (`CloneKey`) from the latest snapshot — vertical-first scaling for a saturated project.
5. **Status** — `phase` (Pending/Warm/Idle/Scaling/Failed), `replicas`, `endpoint`, `lastSnapshot`.
   Status is only written when it actually changes (a busy-loop guard learned the hard way —
   unconditional status writes re-trigger reconcile forever).

Metrics emitted: `buildcat_routes_total`, `buildcat_route_duration_seconds`,
`buildcat_coldstarts_inflight`, `buildcat_scale_events_total`, `buildcat_snapshots_total`.

## Control-plane HA

`buildd` runs `replicas: 2` with **leader election** (`--leader-elect`, a `coordination.k8s.io`
Lease). Two roles, split deliberately:

- **The reconciler runs on the leader only** — exactly one writer of cluster state, no double
  reconcile.
- **The `/route` HTTP API runs on every replica** — the route server sets
  `NeedLeaderElection() = false`, so a routing request is served whether it lands on the leader or a
  follower. Routing is read-mostly (ensure-or-wait); only the leader mutates.

Verified live: `buildcat-buildd` reports `2/2` ready and the `buildcat-buildd.buildcat.dev` Lease is
held by one of the two pods. Kill the leader and the follower takes the Lease; `/route` never stops
serving.

## Public gateway mode

By default daemon Services are `ClusterIP` (in-cluster clients only). For an **external** CI runner,
buildd runs in **gateway mode** (`--daemon-service-type=LoadBalancer`): the daemon Service is a
LoadBalancer and `/route` returns the **external** ingress (`endpointFor` waits up to 90 s for the
LB IP and returns `tcp://<lb-ip>:1234`). The runner then dials that IP directly over mTLS.

This requires the daemon certificate's SAN to cover the public address — see
[ci-integration.md](ci-integration.md) and [security.md](security.md). It is the same shape the
existing `buildkit-service` uses (public LB + mTLS); buildcat just provisions the LB per daemon and
hands back the address through the routing API.

## What stays vanilla (non-goals)

- No fork of BuildKit or containerd; no custom snapshotter; no merging bbolt stores between daemons.
- No concurrently-writable cache **between** daemons — that does not exist in BuildKit. Across
  daemons we share **layers** (via S3, see [storage-and-cold-cache.md](storage-and-cold-cache.md));
  `RUN --mount=type=cache` mounts stay per-daemon by design.
