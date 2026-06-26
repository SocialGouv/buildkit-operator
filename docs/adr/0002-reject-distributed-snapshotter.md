# 0002 — Reject building a distributed containerd snapshotter + CAS

**Status:** Accepted · **Date:** 2026-06-26 (backfilled)

## Context

The most ambitious way to give BuildKit a shared, resilient cache is to build a **distributed
containerd snapshotter** backed by a content-addressable store (CAS): local NVMe/PVC hot cache,
write-through to S3/MinIO, Postgres/etcd metadata + leases, a locality-aware scheduler, distributed
GC, lazy/chunked materialization. This was written up as a full plan
(`.plans/distributed-snapshotter-plan.md`) — "the perceived speed of a local BuildKit cache with the
resilience and pooling of a distributed cache."

## Decision

**Do not build it.** Treat it as an explicit **non-goal**. Deliver the same user-visible outcome —
warm shared cache, resilience, pooling — with the vanilla control-plane approach
([0001](0001-control-plane-over-vanilla-buildkit.md)): one hot daemon per project (hot local cache),
a retained PVC for warmth across scale-to-zero ([0003](0003-scale-to-zero-retained-pvc.md)), in-use
VolumeSnapshots for durability/seeding, and an **opt-in S3 cold cache** for cross-daemon/cold layer
sharing.

## Alternatives considered

- **Build the distributed snapshotter (MVP v1 → production).** Rejected on cost/risk:
  - The plan's own estimate is **~4–6 months to production** (MVP v1 6–8 weeks, v2 8–12 weeks, then
    concurrency/lazy-pulls/multi-tenant/HA on top).
  - Its own **"Risques majeurs"** are load-bearing: *performance illusion* (remote ≠ local),
    *snapshot explosion* (millions of snapshots → aggressive dedupe + GC required), *BuildKit
    overlayfs assumptions* (a compatibility test matrix), *multi-tenancy cache poisoning*.
  - It owns a stateful distributed system (CAS + Postgres + locks + GC) — a far larger operational
    and correctness surface than orchestrating stock daemons.
- **Fork BuildKit/containerd** to add the semantics directly. Rejected for the same reason as in
  [0001](0001-control-plane-over-vanilla-buildkit.md): permanent divergence from upstream.

## Consequences

- ✅ Massively smaller scope and risk; we ship a control plane in weeks, not a distributed storage
  system in quarters, and we **track upstream buildkit/containerd for free**.
- ✅ The phase-0 benchmarks ([benchmarks-phase0.md](../benchmarks-phase0.md)) validated that the
  vanilla path *meets the goal*: retained-PVC reattach is ~31 s (not a rebuild), CoW snapshot/clone
  is constant-time, and OVH supports **in-use** snapshots — so durability needs no scale-to-zero.
- ⚠️ We do **not** get a single concurrently-writable cache across daemons. Cross-daemon sharing is
  **layers via S3** ([storage-and-cold-cache.md](../storage-and-cold-cache.md)), and `RUN
  --mount=type=cache` stays per-daemon. Accepted: it covers the real cold-start cost without owning a
  CAS.
- ↩️ Reversible in principle: the snapshotter could still be built later as a drop-in containerd
  plugin without changing the control plane's contract. The plan is retained under `.plans/` as a
  parked option, not a roadmap item.
