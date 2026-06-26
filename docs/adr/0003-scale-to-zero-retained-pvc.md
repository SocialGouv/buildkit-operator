# 0003 — Scale-to-zero with a retained Cinder PVC (attach, not restore)

**Status:** Accepted · **Date:** 2026-06-26 (backfilled)

## Context

A hot `buildkitd` per project ([0001](0001-control-plane-over-vanilla-buildkit.md)) is expensive if
every idle project keeps a pod (and its CPU/RAM reservation) forever — most projects build in bursts,
then go quiet for hours. But the value of the daemon *is* its warm cache (layers + `RUN
--mount=type=cache` + buildkit state). We need to release compute when idle **without** losing the
cache.

Phase-0 benchmarks measured the relevant costs on OVH Cinder gen2: reattaching a retained volume to a
fresh pod is **~31 s p50** (bench B), and a cold burst of many attaches serialises into minutes
(bench C).

## Decision

Render each daemon as a `StatefulSet`-of-1 whose cache lives on a **`volumeClaimTemplate` PVC**, and
**scale the StatefulSet to 0** when the project is idle past `idleTimeoutSec` (and no build is in
flight) — **keeping the PVC**. Waking the project is a volume **attach** to a new pod, not a cache
**restore**. The `hot` tier opts out (never scales to zero). A `--max-cold-starts` semaphore caps
concurrent attaches; `/prewarm` lets CI hide the attach latency by waking on push.

## Alternatives considered

- **Keep every daemon warm (no scale-to-zero).** Rejected: unbounded idle compute cost as projects
  accumulate; the whole point is bursty workloads.
- **Delete the PVC on scale-down, rebuild on next build** (stateless daemons + S3/registry cache only).
  Rejected: every wake is a full cold rebuild from the remote cache — far slower than a ~31 s attach;
  loses the local cache-mount store entirely. (S3 is kept as a *cold* backstop, not the hot path.)
- **One big shared cache volume** across daemons. Rejected: BuildKit has no concurrently-writable
  shared cache, and it reintroduces cross-project coupling ([0001](0001-control-plane-over-vanilla-buildkit.md)).

## Consequences

- ✅ Idle projects cost **zero compute**; a wake is a quick attach, so the warm cache survives across
  idle periods transparently.
- ✅ Durability/seeding compose cleanly on top: **in-use** VolumeSnapshots (OVH supports snapshotting a
  mounted volume, so no scale-to-zero needed to snapshot) and **CoW clones** for fan-out — both
  constant-time per bench D.
- ⚠️ A wake pays the attach latency (~31 s p50, worse under a burst). Mitigated by `--max-cold-starts`
  backpressure + `/prewarm` + the `hot` tier for latency-critical projects.
- ⚠️ Correctness depends on a **clean volume detach**: the companion drains on SIGTERM (flip readiness
  → settle → exit 0) so Cinder `NodeUnstage` sees a quiescent volume — otherwise a torn cache. This is
  why the companion exists.
- ⚠️ One PVC per project accumulates storage; idle projects still hold their volume. Accepted (storage
  ≪ compute), and bounded operationally by deleting stale `BuildProject`s.
