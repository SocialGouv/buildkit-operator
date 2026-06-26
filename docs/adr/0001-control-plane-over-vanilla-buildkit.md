# 0001 — Control plane over vanilla BuildKit, one hot daemon per (project, arch)

**Status:** Accepted · **Date:** 2026-06-26 (backfilled)

## Context

We need a build service that gives every project a **warm, shared cache** (layers + `RUN
--mount=type=cache`) without the failure modes of a single shared `buildkitd`: cross-project cache
poisoning, noisy-neighbour contention, and a single trust domain. The incumbent `buildkit-service`
runs a handful of shared daemons behind consistent hashing; concurrent builds of unrelated projects
land on the same daemon and fight over one cache.

BuildKit/containerd already do the hard parts well **inside one daemon**: concurrent solves share
layers, dedup cache mounts, and GC by byte budget. What is missing is *orchestration* — making sure
the builds that *should* share a cache reach the *same* daemon, and managing that daemon's lifecycle.

## Decision

Build a **Kubernetes control plane over stock buildkit/containerd** that runs **one hot `buildkitd`
per `(project, arch[, target, name])`**, materialised as a `StatefulSet`-of-1 + `Service` + retained
PVC, reconciled from a `BuildProject` CRD.

The keystone is a **pure routing function** ([`internal/router`](../../internal/router)) shared
verbatim by the CLI and the control plane: `ProjectKey(repo, name, target, arch)` →
`"p" + sha256(...)[:16]`. All builds that must share a cache resolve to the same key ⇒ the same
StatefulSet ⇒ the same daemon. The key is **coarse on purpose** (no branch, no commit) so concurrent
builds and a build an hour later converge; `target`/`arch` are in the key because their caches
genuinely differ; the optional monorepo `name` is omitted from the hash when empty (migration-safe).

Everything at the storage layer stays **vanilla**: no fork of BuildKit/containerd, no custom
snapshotter, no merging bbolt stores between daemons (see [0002](0002-reject-distributed-snapshotter.md)).

## Alternatives considered

- **One shared daemon (the incumbent model).** Rejected: shared writable cache = cross-project
  poisoning + contention + one trust domain. The whole value proposition is *isolation per project*.
- **Fork BuildKit / patch containerd** to add cross-daemon cache semantics. Rejected: enormous
  maintenance burden, and it forfeits the "track upstream for free" property. See
  [0002](0002-reject-distributed-snapshotter.md).
- **Finer routing key (per-branch / per-commit).** Rejected: fragments the cache and defeats the
  point — the cache is most valuable precisely across branches and over time.
- **A generic CI cache (registry/S3 only), no per-project daemon.** Rejected: cold every time; loses
  the hot local layer + cache-mount store that makes incremental builds fast. (S3 is added *on top*
  as a cold-cache, not as the primary mechanism — see [storage-and-cold-cache.md](../storage-and-cold-cache.md).)

## Consequences

- ✅ Per-project isolation (cache, CPU/store, trust) with **zero** storage-layer code to maintain —
  we ride upstream buildkit/containerd.
- ✅ The router being a *pure shared function* means the CLI and control plane can never disagree on
  where a build routes; cache identity is deterministic.
- ✅ Clean substrate for the lifecycle features layered on top: scale-to-zero
  ([0003](0003-scale-to-zero-retained-pvc.md)), in-use snapshots, CoW fan-out, fork isolation
  ([0005](0005-kata-clh-for-untrusted-forks.md)).
- ⚠️ More moving objects than one shared daemon: a StatefulSet/Service/PVC per project, plus a
  reconciler and a routing API to operate. Mitigated by the operator pattern (it *is* the management).
- ⚠️ **No** concurrently-writable cache *between* daemons — that does not exist in BuildKit. Across
  daemons we share **layers** (via S3) and `cache` mounts stay per-daemon by design.
- ⚠️ Cold start exists (provision + attach); addressed by [0003](0003-scale-to-zero-retained-pvc.md)
  + `--max-cold-starts` backpressure + `/prewarm`.
