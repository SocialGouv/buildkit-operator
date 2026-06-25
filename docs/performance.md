# Performance

All numbers below were measured on the `ovh-dev` OVH MKS cluster (GRA9, Cinder gen2) against live
daemons over `buildx` remote + mTLS, wall-clock timed around `docker buildx build --progress plain`,
with `CACHED` step counts read from the build log. They are illustrative single-run measurements on
a shared cluster, not a controlled benchmark suite — but the gaps are large and repeatable.

## Experiment A — buildkit-operator vs the shared `buildkit-service` (warm)

**Context:** a Node build whose `npm install` runs under `RUN --mount=type=cache`, with an `ARG
BUST` to force the `RUN` to re-execute each time — so the measurement isolates **cache-mount reuse
under the real daemon**, not layer skipping. Two consecutive builds per target.

| Target | warm build | notes |
|---|---|---|
| **buildkit-operator** (dedicated daemon, public LB) | **≈ 9.6 s** | the cache mount is hot; no other tenant on the daemon |
| **buildkit-service** (shared pool, HPA 3–6, public LB) | **≈ 18.3 s** | same engine, but a **shared** pod under production load, and consistent-hash routing may land the two builds on different pods |

Same BuildKit engine, same security posture — the ~2× gap is **isolation**: a dedicated daemon has
no noisy neighbours and a single hot store, whereas the shared pool contends for CPU/store and can
route successive builds to different pods (cache miss).

## Experiment B — with vs without the S3 cold cache

**Context:** a heavier Node build (`npm install` of express/lodash/typescript/webpack/react/axios/
commander/jest **as a layer**, plus `apk add build-base git python3 make g++`), on **fresh** daemons
so "cold" means an empty local store. The S3 backend is an in-cluster MinIO; layers exported with
`--cache-to type=s3,mode=max`, imported with `--cache-from type=s3`.

| Daemon state | without S3 | with S3 | delta |
|---|---|---|---|
| **warm** (local cache present) | 2.7 s | 3.1 s | +0.4 s (`cache-to` overhead) |
| **cold** (empty local store) | **41.8 s** (0 CACHED, full rebuild) | **4.5 s** (4 CACHED, rehydrated from S3) | **≈ 9×** |

One-time seed (first cold build that fills the bucket): ≈ 60–76 s. Full analysis in
[storage-and-cold-cache.md](storage-and-cold-cache.md).

## The combined picture

| Scenario | buildkit-service (shared, no S3) | buildkit-operator |
|---|---|---|
| **warm steady state** | ≈ 18 s | ≈ 10 s (dedicated, no contention) |
| **cold daemon** (new project, rebalance, new cluster) | ≈ 42 s (full rebuild — no S3 to fall back on) | **≈ 4.5 s** (rehydrate from S3) |

- **Warm:** buildkit-operator is faster mostly because the daemon is dedicated.
- **Cold:** the gap widens dramatically because buildkit-operator can rehydrate from S3 and the shared service
  cannot — it has no S3 or registry cache layer at all.

## Caveats

- Single-cluster, single-run, shared test cluster — treat as order-of-magnitude, not SLA.
- The S3 numbers depend on bucket locality and object size; OVH Object Storage in-region will differ
  from the in-cluster MinIO test backend.
- buildkit-operator carries a **cold-start** cost the always-on shared service does not: provisioning a fresh
  daemon is ≈ 90 s (image pull + rootless init + PVC attach). That is mitigated by the warm pool,
  `/prewarm`, PVC retention across scale-to-zero, and S3 — see [benchmarks-phase0.md](benchmarks-phase0.md).
