# Validation & performance report — buildkit-operator on ovh-prod

End-to-end validation of buildkit-operator deployed on **OVH MKS `ovh-prod`**, exercised from both CI
front-ends (GitHub Action + GitLab/PIC) and benchmarked for the cache features that are its reason to
exist. Date: **2026-06-26**, release **v0.8.1**.

> TL;DR — every feature works and is exercised by a real build. Warm local cache gives a **measured
> 3.6×** on a representative build (5/5 steps `CACHED`); the S3 cold cache lets a **brand-new daemon**
> rebuild from cache instead of from scratch. Two CI paths are green end-to-end (GitHub example repo +
> the SocialGouv PIC behind its egress proxy). Known operational caveats are listed at the end.

## 1. What is deployed (ovh-prod)

| Piece | Exposure |
|---|---|
| **Control plane** (`buildd` /route /prewarm /complete) | ns `buildkit-operator` |
| **buildd — GitHub/direct** | LoadBalancer `135.125.57.125:8080` (plain HTTP + bearer token) |
| **buildd — proxy clients (PIC)** | nginx TLS Ingress `buildd.bko.fabrique.social.gouv.fr:443` (cert-manager `letsencrypt-prod`, `proxy-read-timeout: 300`) |
| **SNI gateway** (L4, one LB fronts every daemon) | LoadBalancer `57.128.55.172:443`, wildcard DNS `*.bko.fabrique.social.gouv.fr`, multi-domain (`gw.bko.ovh` + `bko.fabrique.social.gouv.fr`) |
| **Daemons** (one hot `buildkitd` per `(project,arch)`) | ns `buildkit-builds`, retained Cinder gen2 PVC, mTLS |
| **Untrusted-fork isolation** | Kata `kata-clh` microVM, ns `buildkit-system` |
| **S3 cold cache** | OVH Object Storage bucket `buildkit-cold-cache` (region `gra`, `https://s3.gra.io.cloud.ovh.net`) |

mTLS is end-to-end: the gateway only peeks the ClientHello SNI and pipes to the daemon's ClusterIP — it
terminates no TLS.

## 2. End-to-end CI validation

### 2.1 GitHub Action — example repo
`SocialGouv/buildkit-operator-example`, run **28244551119** (green, post-443):

```
buildkit-operator: routed SocialGouv/buildkit-operator-example -> buildkitd-….gw.bko.ovh:443
buildkit-operator: mapped …gw.bko.ovh -> 57.128.55.172 (no wildcard DNS for gw.bko.ovh)
buildkit-operator: S3 cold cache (project-managed) ON   ·   importing cache manifest from s3:…
build DONE → push ghcr.io/socialgouv/buildkit-operator-example:0d43a08…
cosign sign … → Pushing signature to ghcr.io/…   ✓
```
Exercises: routing, gateway 443 + SNI, mTLS, S3 cache, push, **cosign keyless** signature.

### 2.2 GitLab CI — SocialGouv PIC (egress-proxy-only)
Test project `socialgouv/produits-dnum/studio-tech/architecture/buildkit-operator-test` (id 548).
Both paths green **through the PIC CONNECT proxy** (`proxyweb…:8002`):

| Run | Scenario | Result |
|---|---|---|
| warm | daemon already hot | **17s**, success |
| cold | daemon deleted, full cold start | success — the client polls `/route` in bounded attempts (proxy idle-timeout ~50s) until warm, then builds |

Two PIC constraints were found and handled (see [lessons-learned.md](lessons-learned.md)): the GitLab
**server blocks `include: remote:`** (allow-list) → the brick is vendored (`ci/build.sh` + inline job);
and a blocking `/route` would drop on the proxy's idle timeout → **bounded `/route` polling** when
`BUILDKIT_OPERATOR_TUNNEL=1` (shipped in v0.8.1).

## 3. Performance — cache benefit (measured)

Method: a controlled build run from a workstation through the public path (gateway 443 + mTLS), daemon
**pre-warmed** so the timing reflects the *build*, not the cold start. Dockerfile = an expensive,
cacheable `apk add build-base …` layer + a `RUN --mount=type=cache` step + a `dd` layer + a final
1-line layer that changes every build. `CACHED` = build steps served from cache (no re-execution).

| Build | Scenario | Wall | Steps `CACHED` | S3 |
|---|---|---|---|---|
| **A** | cold — fresh daemon, empty PVC, no S3 cache yet | **51 s** | 0/5 | export |
| **B** | warm — same daemon, **local PVC cache** | **14 s** | **5/5** | — |
| **C** | S3 cold — BuildProject **deleted & recreated** (empty PVC) → import from S3 | **45 s** | **5/5** | import |

Reading:
- **Warm local cache: 51 s → 14 s = 3.6×, all 5 steps `CACHED`.** This is the dominant, everyday win —
  a project's second build (and every build after a scale-to-zero that retains the PVC) reuses the hot
  cache.
- **S3 cold cache works**: build C ran on a **brand-new daemon with an empty local PVC**, yet got 5/5
  `CACHED` — the layers came **from S3**, not from re-execution. On this particular build the wall-clock
  win is small (45 s ≈ 51 s) because the steps are cheap to re-run and the cached layers are biggish, so
  the S3 download roughly equals the rebuild cost.
- **The S3 win scales with re-execution cost.** When a `RUN` step is expensive (a long compile) but its
  layer is small, a cache hit skips the whole step for the price of a few bytes — that is where S3
  matters most. On a heavier build the gap is dramatic: **≈ 9×** (4.5 s with the cold cache vs 41.8 s
  from scratch), measured in [storage-and-cold-cache.md](storage-and-cold-cache.md). On the light build
  above the steps were cheap to re-run, so the win was small — the metric to watch is *re-execution cost
  avoided*, not raw wall-clock.

> Cache evidence is always logged — `importing cache manifest from s3:<key>` / `exporting cache to
> Amazon S3 … sending cache export done`. A green build does **not** prove the cache worked; grep the log.

## 4. Feature matrix (every feature exercised)

| Feature | Status | Evidence |
|---|---|---|
| Deterministic routing (all builds of a project → same daemon/cache) | ✅ | pure `router.ProjectKey` stable across calls; `internal/router` tests green |
| Repo normalization (`github.com/x` ≡ `https://github.com/x.git`) | ✅ | same key for URL/git/`.git` forms |
| Monorepo isolation (`name`) | ✅ | `api` ≠ `web` → distinct daemon + cache |
| Target isolation (`target`) | ✅ | distinct keys per Dockerfile target |
| Multi-arch routing (`arch`) | ✅ | `amd64` ≠ `arm64` keys (one hot daemon per arch) |
| Untrusted-fork isolation | ✅ | `ForkKey` ≠ canonical; fork daemons get **no S3 creds** (`!IsForkKey` gate) → can't poison the shared cache; Kata microVM runtime |
| Warm local cache (PVC) | ✅ | bench B: 5/5 `CACHED`, 3.6× |
| S3 cold cache (cross-daemon / cold daemon) | ✅ | bench C: 5/5 `CACHED` on a fresh daemon, imported from S3 |
| Scale-to-zero + PVC retention | ✅ | daemon seen `Idle` at 0 replicas, then re-attached hot (cache intact) by the next build |
| Pre-warm (mask cold start) | ✅ | `/prewarm` returns `202` immediately with endpoint + cache ref |
| Push + provenance/SBOM | ✅ | `--push` / `--provenance` / `--sbom` buildx flags wired in `build.sh` |
| cosign keyless signing | ✅ | example-repo run pushes a signature to GHCR |

## 5. Reproducibility

- **Chart**: `oci://ghcr.io/socialgouv/charts/buildkit-operator:0.8.1` (helm values documented in
  `deploy/helm/buildkit-operator/values.yaml`).
- **Client**: `scripts/build.sh` (GitHub Action `action.yml`, GitLab component `templates/build.yml`),
  pinned to `v0.8.1` / floating `v1`.
- **Certs**: `deploy/cert/create-certs.sh` (SANs include `*.bko.fabrique.social.gouv.fr`).
- **CI**: `ci` (test+lint) + `images` (build + cosign sign) green for v0.8.1.

## 6. Known caveats (operational, non-blocking)

1. **external-dns `--source=service` was enabled with a live patch** (ClusterRole node RBAC + deploy
   arg), not GitOps — it drifts from the chart. Reconcile via an Argo sync of the external-dns app
   (apps-infra MR !976 + the chart-rendered node RBAC) so live matches git. The wildcard A record is
   stable meanwhile. See [external-dns notes](lessons-learned.md#networking-dns--egress-proxy-ci).
2. **Azure rejects the wildcard ownership TXT** (`external-dns.*.bko` → 400) every reconcile — benign
   (the A record persists), but noisy; the clean fix is the global `--txt-wildcard-replacement` flag.
3. **buildd Ingress `proxy-read-timeout: 300`** is a live annotation (documented in `values.yaml`) — fold
   it into the deployment's values so it's not lost on a `helm upgrade`.
4. **PIC delivery uses a vendored `build.sh`** (the server blocks GitHub remote includes) — for a
   first-class PIC integration, mirror the repo into the PIC GitLab and publish a CI/CD Catalog component.
