# buildcat vs the shared `buildkit-service`

A side-by-side with the existing BuildKit service running in the fabrique infra (namespace
`buildkit-service` on `ovh-prod`), gathered by read-only inspection of its live workloads and chart.
The goal is an honest accounting — what each design wins, and where they are simply the same.

## What `buildkit-service` is

A **shared pool** of rootless buildkit pods:

- `StatefulSet`, observed `5/5`, fronted by an **HPA (3–6 replicas)**.
- **Consistent-hash routing** (the upstream `examples/kubernetes/consistenthash` pattern) spreads
  repos across pods so the same repo tends to hit the same pod.
- **Eight public LoadBalancers** + mTLS (`buildkit.socialgouv.io`), client certs committed in-chart.
- Cache is **per pod, shared across all repos** that hash to it. **No** S3 / registry cache layer.
- Security posture: rootless, `Unconfined` seccomp/AppArmor, `allowPrivilegeEscalation` unset —
  **identical** to buildcat's daemon (it is the same incompressible rootless requirement).

## Side-by-side

| Axis | buildkit-service | buildcat |
|---|---|---|
| **Daemon model** | shared pool (HPA 3–6), consistent-hash | **dedicated per `(project, arch)`** |
| **Cache scope** | per pod, shared by all repos on that pod | **per project** (own daemon + own PVC) |
| **Cross-project poisoning** | possible (shared writable store) | **prevented** (no shared writable cache) |
| **Untrusted / fork PRs** | same write access as trusted builds | **isolated** ephemeral daemon, read-only seed, no write-back |
| **Warm build** (measured) | ≈ 18 s (shared, under prod load) | **≈ 10 s** (dedicated, no contention) |
| **Cold / rebalanced build** | ≈ 42 s — full rebuild, no fallback | **≈ 4.5 s** — rehydrate from S3 |
| **Distributed / durable cache** | none | **S3 layers + VolumeSnapshots** |
| **Scale-to-zero** | no (always on) | yes (PVC retained on wake) |
| **Daemon security posture** | rootless + Unconfined | **identical** (same rootless constraint) |
| **Control-plane HA** | n/a (no operator) | leader election, 2 replicas, `/route` on all |
| **Cold-start cost** | none (always warm) | ≈ 90 s to provision a fresh daemon |
| **Public surface** | 8 fixed LBs | 1 LB per exposed daemon (gateway mode) |

## What each wins

**buildcat wins on:**

- **Isolation** — both security (no cross-project poisoning, fork isolation) and performance (no
  noisy neighbours). This is the structural advantage of a daemon per project.
- **Cold-start resilience** — S3 rehydration (≈ 9×) where the shared service must rebuild from
  scratch. It simply has no cold-cache layer.
- **Durability** — VolumeSnapshots make a project's cache survive the PVC, the pod, and the cluster
  (restore-from-snapshot for DR / migration).
- **Cost under sparse load** — scale-to-zero with PVC retention; idle projects cost storage, not
  compute. The shared pool keeps 3–6 pods hot regardless.
- **Operability** — a real operator with metrics, leader-elected HA, and a routing API.

**buildkit-service wins on:**

- **Cold-start** — it is always warm, so there is never a 90 s provision wait. buildcat mitigates
  this (warm pool, `/prewarm`, PVC retention, S3) but does not eliminate it.
- **Simplicity** — a StatefulSet + HPA + consistent-hash is less machinery than a CRD + reconciler +
  snapshots + fan-out.
- **Fixed, known surface** — a stable set of endpoints rather than per-daemon LBs.

## What is the same (don't oversell)

- **Daemon hardening.** Both relax `no_new_privs` for rootless buildkit. buildcat does not make the
  daemon more locked down — see [security.md](security.md).
- **The build engine.** Both run vanilla `buildkitd`. Warm-cache-hit builds of the same thing on an
  uncontended daemon are comparable; the measured gaps come from isolation and cold-start handling,
  not a faster engine.

## When to pick which

- **Many small repos, latency-sensitive, security-sensitive (forks, multi-tenant):** buildcat — the
  isolation and cold rehydration pay off, and scale-to-zero controls cost.
- **A few always-busy repos where 90 s cold-starts are unacceptable and isolation is a non-issue:**
  the shared pool's always-warm simplicity is hard to beat.

The two are not mutually exclusive: buildcat's gateway + mTLS shape is deliberately the same as
buildkit-service, so a CI integration can point at either.
