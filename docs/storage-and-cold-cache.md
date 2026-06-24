# Storage layers & the S3 cold cache

buildcat has **three** cache layers, each at a different point on the speed/durability curve. The
fast path is local; durability and cold-start resilience are added without slowing the fast path.

| Layer | Backing | Scope | Speed | What it holds |
|---|---|---|---|---|
| **Warm** | Cinder gen2 PVC (one per daemon) | per `(project, arch)` | fastest (local) | the live `buildkitd` store — **layers + `RUN --mount=type=cache` mounts** + bbolt metadata |
| **Durable** | `VolumeSnapshot` (in-use snapclass) | per project | restore = an attach | point-in-time copies of the warm PVC, for DR / new cluster / CoW fan-out |
| **Cold / distributed** | **S3** (buildx `type=s3`) | shared across daemons & clusters | network | **layers only** (build-step results) — the cross-daemon sharing BuildKit otherwise can't do |

The warm PVC is **retained across scale-to-zero** (M2), so an idle project wakes by re-attaching its
own cache, not by rebuilding. Snapshots (M3) make that cache survive the PVC itself. S3 is the layer
that lets a **brand-new or wiped** daemon avoid a from-scratch build.

## The S3 cold cache

### It is external and opt-in — buildcat is an S3 *client*, not an S3 *provider*

buildcat does **not** deploy or bundle an object store. It **consumes** an S3-compatible bucket the
same way it consumes a container registry — you provide it. In production that is **OVH Object
Storage** (`s3.<region>.io.cloud.ovh.net`); for the autonomous proof in this session it was a
throwaway MinIO. There is no MinIO in the architecture; it was a test backend.

### S3 is a **project policy**, configured once on buildd — not on every CI caller

The cold cache is **centralized in the control plane** so CI callers configure **zero** S3 (no
flags, no env, no secrets on the client side):

- **buildd holds the bucket config** — flags `--s3-bucket`, `--s3-region`, `--s3-endpoint` (Helm
  values `s3.bucket` / `s3.region` / `s3.endpoint`). An empty bucket disables the cold cache.
- **`/route` returns the per-project cache reference** in `RouteResponse.Cache`: the bucket, region,
  endpoint, and a `name` = the **project key** (the per-project cache prefix). It carries **NO
  credentials**.
- **The client applies it automatically.** The `build` CLI (and `build.sh`) read `RouteResponse.Cache`
  and add `--cache-from type=s3,… --cache-to type=s3,…,mode=max` themselves — no S3 flags, no S3 env
  on the caller.
- **The AWS credentials live on the daemon pods**, not on the wire and not on the runner: a k8s
  Secret (`--s3-creds-secret`, Helm `s3.credsSecret`) holding `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY`, mounted as env on the daemons. buildkit's s3 backend falls back to the
  daemon's AWS env when the client passes no creds — which it now never does.

```
# on buildd / the Helm chart (the bucket config + creds for the cold cache)
--s3-bucket        / s3.bucket        shared bucket name (empty = disabled)
--s3-region        / s3.region        region (default us-east-1)
--s3-endpoint      / s3.endpoint      endpoint URL (OVH Object Storage / MinIO; empty = AWS default)
--s3-creds-secret  / s3.credsSecret   k8s Secret (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) → daemon env
```

### The daemon does the S3 I/O — the runner never touches S3

With the `buildx` **remote** driver, cache export/import for `type=s3` runs **inside `buildkitd`**,
not in the client. The client only passes the (credential-free) reference through. Consequences
proven this session:

- The S3 endpoint is resolved **daemon-side**, so it can be an **in-cluster** address
  (`minio.buildcat.svc:9000`) that the external GitHub-hosted runner cannot even reach. The runner
  never opens an S3 connection. In the example CI log:
  `#8 importing cache manifest from s3:12475883348330026593` — the in-cluster daemon did it.
- S3 credentials live on the **daemon pods** (a k8s Secret mounted as AWS env), never on the runner
  and never on the wire; rotating them is a Secret change in the cluster, not a CI-secret change.

### What S3 covers (and what it doesn't)

- ✅ **Layers** — the results of build steps (`RUN`, `COPY`, …) exported with `mode=max` (all
  intermediate layers, not just the final image).
- ❌ **`RUN --mount=type=cache` mounts** — these stay per-daemon. They are not part of the image
  graph and are not exported to S3. They are served by the warm PVC.
- ❌ **The base-image pull** — that is the registry's job; a cold daemon still pulls `FROM` from the
  registry regardless of S3.

So S3 turns "rebuild every slow `RUN` from scratch" into "import the resulting layer". That is the
expensive part of a cold build.

## Measured value

Identical context (a heavy Node build — `npm install` of express/lodash/typescript/webpack/react/
axios/commander/jest as a layer, plus `apk add build-base git python3 make g++`), on `ovh-dev`,
fresh daemons:

| Daemon state | without S3 | with S3 | delta |
|---|---|---|---|
| **warm** (local cache present) | 2.7 s | 3.1 s | **+0.4 s** — the `cache-to` export overhead, negligible |
| **cold** (empty local cache) | **41.8 s** (full rebuild, 0 CACHED) | **4.5 s** (rehydrate, 4 CACHED, `importing cache manifest from s3`) | **≈ 9× faster** |

One-time **seed** cost (the first cold build that populates the bucket): ≈ 60–76 s.

Reading:

- **Warm builds don't get faster with S3** — the local cache already serves them. S3's job on a warm
  build is to **keep the bucket fresh** (`cache-to`), for ~free.
- **Cold builds get ~9× faster** — the slow `RUN` layers are imported instead of recomputed.
- **When does "cold" actually happen?** New project, lost/GC'd PVC, a new cluster (DR/migration), or
  a cache eviction. buildcat **retains the PVC across scale-to-zero**, so cold is *rarer* than on a
  service that drops local cache on rebalancing — and S3 covers the rest.

End-to-end proof: `socialgouv/buildcat-example` CI run `28126430796` (green) on a stock
GitHub-hosted runner exported and imported its layer cache through the in-cluster daemon to S3.

## Why this matters versus the shared service

The existing `buildkit-service` has **no** S3 (and no registry) cache layer — verified by grepping
its chart. Every cold pod there (HPA scale-up, consistent-hash rebalance, restart) pays the full
from-scratch path. S3 is precisely the cold-start answer it lacks. See
[comparison-buildkit-service.md](comparison-buildkit-service.md) and [performance.md](performance.md).
