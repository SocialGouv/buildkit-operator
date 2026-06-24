# buildcat

**A distributed BuildKit build service: one hot, *vanilla* `buildkitd` per `(project, arch)` on Kubernetes.**

buildcat gives CI builds the perceived speed of a warm local BuildKit cache, with the
elasticity and durability of Kubernetes — **without forking BuildKit, containerd, or writing a
custom snapshotter.** It is a small **control plane** (routing + lifecycle) on top of stock
`buildkitd`/`containerd`. Built for OVH Managed Kubernetes (Cinder gen2), portable to any CSI.

> Status: milestones M1–M5 implemented, unit-tested, and **proven end-to-end on a real OVH MKS
> cluster** (daemon routing, warm-cache reuse, scale-to-zero, in-use snapshots + restore,
> observability, fork isolation, CoW fan-out). buildkitd stays unmodified.

---

## Documentation

This README is the overview. The [`docs/`](docs/) directory holds the deep-dives and the **measured
evidence** gathered validating buildcat on a real OVH MKS cluster:

- [architecture.md](docs/architecture.md) — routing key, reconcile loop, HA, gateway mode
- [security.md](docs/security.md) — rootless constraint, Kyverno fix, threat model, fork isolation
- [storage-and-cold-cache.md](docs/storage-and-cold-cache.md) — the 3 cache layers; **S3 ≈ 9× cold**
- [performance.md](docs/performance.md) — measured warm/cold, with/without S3
- [comparison-buildkit-service.md](docs/comparison-buildkit-service.md) — side-by-side vs the shared service
- [ci-integration.md](docs/ci-integration.md) — CI-agnostic integration + public exposure
- [benchmarks-phase0.md](docs/benchmarks-phase0.md) — the Cinder gen2 bench that picks the config
- [operations.md](docs/operations.md) — deploy / expose / observe / tear down runbook

---

## Why this design

The core insight: **concurrency and cache sharing are free if they stay inside a single
daemon.** A `buildkitd` instance has one local store (content + snapshots + bbolt metadata), so:

- two concurrent builds of the same project **share layers _and_ `RUN --mount=type=cache`
  cache mounts**, and **dedup in-flight** — for free, internally;
- we never touch the storage layer. We attack **routing** (send builds that should share a cache
  to the *same* daemon) and **lifecycle** (keep it warm, scale it to zero, snapshot it, clone it).

This is validated against the BuildKit source: cache mounts are keyed by mount id (not
build/session id) in a daemon-wide pool, and identical solves merge in the scheduler. So the value
add is **good Kubernetes orchestration + the stock BuildKit client**, not low-level systems code.

The rejected alternative — a custom distributed snapshotter / CAS — is explicitly a non-goal: it
sits at the wrong layer and does not deliver shared cache mounts.

---

## Architecture

```
   client (CI)                          Kubernetes (one namespace)
  ┌───────────┐   POST /route        ┌──────────────────────────────────────────┐
  │  build    │ ───────────────────► │  buildd (Deployment)                      │
  │  (CLI)    │ ◄─── endpoint ────── │   • controller-runtime manager            │
  └─────┬─────┘                      │   • reconciles BuildProject -> daemon     │
        │ buildx remote (mTLS)       │   • HTTP API: /route /prewarm             │
        ▼                            │   • Prometheus /metrics                   │
  ┌───────────────────────┐         └───────────────┬──────────────────────────┘
  │ StatefulSet-of-1       │  reconciles            │ creates / scales
  │  buildkitd (vanilla)   │ ◄──────────────────────┘
  │  + companion (sidecar) │        one per (project, arch)
  │  + gen2 PVC (warm cache)│       Service :1234 (TCP + mTLS)
  └───────────────────────┘
```

**Components** (this repo is a Go monorepo):

| Path | What |
|---|---|
| [`api/v1alpha1`](api/v1alpha1) | CRD: `BuildProject` (cache identity + daemon lifecycle) |
| [`internal/router`](internal/router) | the **stable routing key** — `ProjectKey(repo,target,arch)`; pure, shared by CLI and control plane |
| [`internal/builder`](internal/builder) | renders the `buildkitd` `StatefulSet` + `Service` (gen2 PVC, security profiles) |
| [`internal/controller`](internal/controller) | the `BuildProject` reconciler (scale, snapshot, fan-out, status) |
| [`internal/metrics`](internal/metrics) | Prometheus collectors |
| [`cmd/buildd`](cmd/buildd) | the control plane (manager + `/route` API) |
| [`cmd/companion`](cmd/companion) | per-daemon sidecar: readiness, heartbeat, inode-GC backstop, clean drain |
| [`cmd/build`](cmd/build) | the client CLI — a thin, drop-in-ish `docker build` |
| [`deploy/`](deploy) | Helm chart, generated CRDs/RBAC, mTLS cert script, `buildkitd.toml`, warm-pool |

**Routing rule (critical):** all builds that must share a cache **must resolve to the same key**
⇒ the same StatefulSet ⇒ the same daemon. The key is `"p" + sha256(normRepo ⏎ normTarget ⏎
normArch)[:16]` — coarse on purpose (no context, no branch) so concurrent and later builds
converge. A too-fine key fragments the cache and kills sharing.

---

## Features (by milestone)

| | Feature | Notes |
|---|---|---|
| **M1** | Daemon-per-project, routed | `BuildProject` → STS-of-1 + Service + gen2 PVC + mTLS; build via `buildx remote`. Concurrent builds share layers **and** cache mounts. |
| **M2** | Elasticity | tier-aware **scale-to-zero** when idle (`IdleTimeoutSec`), the PVC is **retained** (no restore on wake); `/prewarm` webhook; preemptible **warm pool** so wake-ups don't trigger node autoscaling. |
| **M3** | Durability & cold cache | periodic **in-use** `VolumeSnapshot` (no scale-to-zero needed on OVH), **restore-from-snapshot** (`spec.restoreFromSnapshot`), inode-GC backstop, S3 cold cache via the CLI. |
| **M4** | Observability, security, backpressure | Prometheus metrics; **fork-PR isolation** (untrusted builds get an ephemeral daemon seeded read-only, no write-back — anti cache-poisoning); **cold-start rate limit** (`--max-cold-starts`). |
| **M5** | Conditional fan-out | `spec.Fanout` materializes N warm **CoW clone** daemons from the latest snapshot for a saturated project — vertical scaling (Resources/`CacheVolumeGi`) stays the first resort. |

---

## The `BuildProject` resource

```yaml
apiVersion: buildcat.dev/v1alpha1
kind: BuildProject
metadata:
  name: p1a2b3c4d5e6f7a8        # = spec.key
  namespace: buildcat
spec:
  key: p1a2b3c4d5e6f7a8         # stable cache identity (set by the router)
  repo: github.com/acme/app     # normalized, informational
  target: ""                    # Dockerfile target stage ("" => default)
  arch: amd64                   # amd64 | arm64
  tier: warm                    # hot (never scale-to-zero) | warm | cold
  idleTimeoutSec: 900           # wake window before scale-to-zero (from bench B)
  cacheVolumeGi: 60             # gen2: throughput scales with size
  storageClass: csi-cinder-high-speed-gen2
  snapshotEverySec: 0           # durability snapshot cadence (0 = off)
  restoreFromSnapshot: ""       # seed the cache PVC from a VolumeSnapshot (DR / new cluster)
  fanout: 0                     # M5: extra CoW clone daemons (0 = none)
  securityProfile: rootless     # rootless | userns | privileged
status:
  phase: Warm                   # Pending | Warm | Idle | Scaling | Failed
  replicas: 1
  endpoint: tcp://buildkitd-p1a2b3c4d5e6f7a8.buildcat.svc:1234
  lastSnapshot: snap-...
```

You rarely write these by hand — the `build` CLI / `buildd` `/route` create them on demand.

---

## Install

Prerequisites: a Kubernetes cluster with a CSI that supports snapshots (OVH MKS gen2 +
`csi-cinder-snapclass-in-use-v1`), `kubectl`, `helm`, and the VolumeSnapshot CRDs.

```bash
# 1. CRDs (generated from kubebuilder markers)
make manifests
kubectl apply -f deploy/crd

# 2. mTLS certs (wildcard SAN over the daemon Services)
deploy/cert/create-certs.sh buildcat        # writes the buildkit-daemon-certs / -client-certs Secrets
kubectl -n buildcat apply -f deploy/cert/.certs/*-secret.yaml

# 3. control plane (buildd Deployment + RBAC + buildkitd.toml ConfigMap)
helm upgrade --install buildcat deploy/helm/buildcat -n buildcat --create-namespace

# 4. (optional) warm node-pool headroom
kubectl apply -f deploy/warm-pool.yaml
```

Images (`buildd`, `companion`) are built and pushed to `ghcr.io/socialgouv/buildcat-*` by the
[`images`](.github/workflows/images.yml) GitHub Actions workflow. For a private registry, give the
namespace a pull secret and attach it to the `default` and `buildcat-buildd` ServiceAccounts.

### ⚠️ Admission policy (Kyverno / restricted PSS)

Rootless `buildkitd` needs `allowPrivilegeEscalation` **unset** (its `newuidmap` requires
`no_new_privs` OFF) plus `seccompProfile`/`appArmorProfile: Unconfined`. The pods stay **non-root
and unprivileged** — only `no_new_privs` is relaxed.

If a policy (e.g. Kyverno) **forces** `allowPrivilegeEscalation: false`, rootless buildkit
crash-loops (`newuidmap: Could not set caps`). The fix is to **exempt the daemon namespace** from
that mutate rule (the precedented pattern for CI/build namespaces), or switch `securityProfile` to
`userns`/`privileged`. See [`deploy/README.md`](deploy/README.md).

---

## Usage

```bash
# build via the CLI (resolves the key, routes through buildd, builds via buildx remote+mTLS)
build --repo github.com/acme/app --arch amd64 -t registry/acme/app:sha --push .

# or just talk to the buildd API
curl -XPOST http://buildcat-buildd.buildcat.svc:8080/route   -d '{"repo":"github.com/acme/app","arch":"amd64"}'
curl -XPOST http://buildcat-buildd.buildcat.svc:8080/prewarm -d '{"repo":"github.com/acme/app","arch":"amd64"}'   # on git push
curl -XPOST http://buildcat-buildd.buildcat.svc:8080/route   -d '{"repo":"...","arch":"amd64","untrusted":true}'   # fork PR -> isolated daemon
```

`buildd` HTTP API: `POST /route` (ensure + wait Ready, returns the mTLS endpoint), `POST /prewarm`
(anticipatory scale-up, returns immediately), `POST /heartbeat`, `GET /healthz`, and Prometheus on
`--metrics-addr` (`:8081`).

---

## Development

```bash
make generate     # deepcopy (zz_generated.deepcopy.go)
make manifests    # CRDs + RBAC from markers (controller-gen)
make build        # bin/{buildd,companion,build}
make test         # unit tests (router key normalization, reconciler via fake client)
make docker-build # buildd + companion images
```

Go 1.24+, controller-runtime. The reconciler is unit-tested with the fake client (scale-to-zero,
snapshot cadence, fan-out); the routing key has table tests (the cache-sharing invariant).

To work against the real BuildKit source while developing, clone it into a local `.repos/`
(git-ignored) — having the upstream code local is a deliberate practice here.

---

## Phase 0 benchmark (drives the config)

A `kubectl`-only protocol measures the storage latencies that pick the config values. On OVH MKS
(GRA9, gen2) the run found:

- **Isolated attach** p50 ≈ 19.5 s → scale-to-zero is fine for the *warm* tier **with pre-warm**.
- **Reattach cycle** p50 ≈ 31 s → use a **generous idle timeout** (~15 min); don't churn.
- **Burst** of cold starts degrades into **minutes** (N=50 p50 ≈ 6 min) → **rate-limit wake-ups**.
- **Snapshot + clone** is **CoW** (constant with data size) → **fan-out is viable** (M5).
- **In-use snapshots are supported** → snapshot a hot daemon **without** scale-to-zero (M3).

These feed `idleTimeoutSec`, `--max-cold-starts`, warm-pool size, and the snapshot class.

---

## Non-goals

- ❌ Forking BuildKit / containerd, or writing a custom snapshotter.
- ❌ Merging bbolt stores between daemons.
- ❌ A concurrently-writable cache shared *between* daemons (it doesn't exist in BuildKit). Across
  daemons we share **layers** (S3); cache mounts stay per-daemon.

---

## License

[MIT](LICENSE) © SocialGouv.
