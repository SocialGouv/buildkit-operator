# The build-acceleration landscape & where buildkit-operator fits

> A positioning document: it establishes that there is a real, funded market for accelerating CI
> builds, maps the commercial offerings in the segment, and compares their features to
> buildkit-operator. buildkit-operator is **open-source and self-hosted** — that is its structural
> difference.

## TL;DR

Remote container build is a **funded, competitive, growing** segment: Depot (Y Combinator), Docker
itself (Docker Build Cloud), and a wave of challengers (Blacksmith, WarpBuild, Namespace). They all
solve the same pain: slow CI builds, cross-arch emulation, a cold cache on every job, expensive
runners.

**The demand is therefore proven.** buildkit-operator targets exactly that lane — one hot remote
`buildkitd` per project, persistent cache, native multi-arch — but with an angle **no active
competitor offers**: open-source, self-hosted, sovereign, no vendor lock-in, no metered
build-minutes. On the build technology itself, the **core capabilities are at parity** with the
direct segment; the difference is the delivery model (a self-hostable engine vs a hosted product).

---

## 1. The market proves a real, structural need

Build acceleration is not a niche: it is a segment where multiple companies raise funding and
compete, and where the reference vendor (Docker) entered with its own product.

- **Funded demand** — Depot is a Y Combinator company in continuous expansion (remote build → CI →
  runners → registry → remote cache → agent sandboxes).
- **Docker validates the need** by launching its own managed offering, **Docker Build Cloud**.
- **A wave of challengers** (Blacksmith, WarpBuild, Namespace) compete on fast CI runners + remote
  builders — proof the market is broad enough for several players.

### The clearest signal: Earthly Satellites shutting down

**Earthly stopped its commercial Satellites service on 16 July 2025** (announced 16 April 2025) and
ended active maintenance of its open-source project. Its official recommendation to users who want
to keep the benefit: **"roll out your own remote BuildKit."**

That is exactly buildkit-operator's proposition. Market reading: the demand exists and persists, but
**"build your own remote BuildKit"** is left **orphaned on the turnkey open-source tooling side**.
That is precisely the gap buildkit-operator fills.

### The common pain every offering addresses

- Slow CI builds: cross-arch QEMU emulation, and **a cold cache on every job** (nothing is shared
  between two runs).
- Cost and latency of hosted CI runners.
- No shared layer / cache-mount cache across jobs, branches, or developers.

---

## 2. The landscape

### Direct segment — **container** build acceleration (buildkit-operator's competitors)

| Offering | Model | Key notes |
|---|---|---|
| **Depot.dev** | Proprietary SaaS (US) | The reference. Remote BuildKit, per-project persistent NVMe cache, **native Intel + Arm without emulation**. Expanded into Depot CI (2026), GitHub Actions runners, registry, remote cache (Bazel/Go/Turbo), agent sandboxes. Priced per **build-minute + cache storage GB/month**. |
| **Docker Build Cloud** | Proprietary SaaS (Docker) | Managed remote builders, shared team cache, multi-arch. Developer **$20/mo** (500 build min, 25 GB cache) / Startup **$200/mo** (5,000 min, 250 GB). |
| **Blacksmith** | Proprietary SaaS | Remote BuildKit + GitHub Actions runners; strong on DX and observability. |
| **WarpBuild** | Proprietary SaaS + **BYOC** | Remote Docker builders (claimed "40×"), runners, cache. The BYOC option (your own cloud) drives cost down substantially. |
| **Namespace** | Proprietary SaaS | Remote builders + CI runners, Docker layer cache (billed separately per GB/month). Pay-as-you-go. |
| **Earthly Satellites** | **Shut down (Jul 2025)** | Remote BuildKit service discontinued; now recommends **self-hosting your own remote BuildKit** → exactly our lane. |

> Every **active** player in this direct segment is a **proprietary US SaaS**. None offers a complete
> open-source, self-hosted solution.

### Adjacent segment — general-purpose remote cache / execution (context, **not** direct competitors)

These tools accelerate **task and test caching** (Bazel Remote Execution protocol, or monorepo task
caching), **not container image builds**. They illustrate the breadth of the "build acceleration"
demand, but play in a different lane.

- **BuildBuddy** — open-source (MIT): remote cache + Remote Build Execution for Bazel; OSS core +
  cloud offering.
- **EngFlow** — RBE + remote cache for Bazel, self-hosted or hosted; single-node free tier.
- **Nx Cloud / Turborepo Remote Cache** — task caching for JS/TS monorepos; Nx offers self-hosting
  (S3/GCP/Azure).
- **Dagger** — programmable CI engine **built on BuildKit** (from the Docker/BuildKit lineage); OSS
  engine + Dagger Cloud.

**Scoping**: buildkit-operator plays in the **Depot / Docker Build Cloud** lane (container image
builds), not the Bazel RBE or monorepo task-cache lane.

---

## 3. Feature comparison — buildkit-operator vs the direct segment

Legend: ✅ present · ⚠️ partial / varies by offering · ❌ absent.

| Criterion | buildkit-operator | Depot.dev | Docker Build Cloud | Blacksmith / WarpBuild / Namespace |
|---|---|---|---|---|
| Hot remote BuildKit + persistent cache | ✅ | ✅ | ✅ | ✅ |
| Native multi-arch amd64 + arm64 (daemon per arch, no emulation) | ✅ | ✅ | ✅ | ⚠️ |
| Cache mounts (`RUN --mount=type=cache`) persisted on PVC | ✅ | ✅ | ⚠️ | ⚠️ |
| Remote cold cache (S3 rehydration, ≈ 9×) | ✅ | ✅ (managed NVMe) | ⚠️ | ⚠️ |
| Durable snapshots (DR / cache migration) | ✅ | ⚠️ (managed, opaque) | ❌ | ❌ |
| Scale-to-zero with retained cache | ✅ | ✅ (transparent) | ✅ (transparent) | ⚠️ |
| **Fork/untrusted isolation in a VM (Kata microVM)** | ✅ | ❌ | ❌ | ❌ |
| **Repo-bound OIDC identity** (GitHub/GitLab/Forgejo) | ✅ | ⚠️ | ⚠️ | ⚠️ |
| End-to-end mTLS, rootless, **vanilla buildkit (zero fork)** | ✅ | ❌ (proprietary) | ❌ (proprietary) | ❌ (proprietary) |
| Supply-chain attestations (SLSA provenance + SBOM + cosign) | ✅ | ⚠️ | ⚠️ | ⚠️ |
| **Open-source / self-hosted / data sovereignty** | ✅ | ❌ | ❌ | ⚠️ (WarpBuild BYOC only, still proprietary) |
| **No vendor lock-in / no metered build-minutes** | ✅ | ❌ | ❌ | ❌ |
| Built-in billing / metering | ❌ | ✅ | ✅ | ✅ |
| Self-serve web dashboard (usage, API keys, teams) | ❌ | ✅ | ✅ | ✅ |
| Per-tenant quotas / SLA | ❌ | ✅ | ✅ | ✅ |
| Built-in image registry | ❌ | ✅ | ⚠️ | ⚠️ |

Implementation pointers (repo paths): routing `cmd/buildd/route.go` + `internal/router/router.go`;
OIDC identity `internal/identity/identity.go`; lifecycle / scale-to-zero / snapshots
`internal/controller/buildproject_controller.go`; fork isolation + Kata via
`deploy/helm/buildkit-operator/values.yaml`; CI integration `action.yml`. See also
[architecture.md](architecture.md), [security.md](security.md),
[sandboxed-builds.md](sandboxed-builds.md), [storage-and-cold-cache.md](storage-and-cold-cache.md).

---

## 4. Our differentiation

On **build technology**, buildkit-operator is at **parity** with the direct segment (hot remote
BuildKit, persistent cache, native multi-arch, cold cache, scale-to-zero). Its distinctive
advantages are structural — not something a proprietary SaaS can match:

- **Sovereignty / data residency** — self-hosted on Kubernetes (here OVH, a French cloud). Source
  code, caches, and artifacts never leave your own infrastructure. A **decisive argument for the
  public sector** (`social.gouv.fr`) that no US SaaS (Depot, Docker, Blacksmith, WarpBuild,
  Namespace) can equal.
- **Open-source & vanilla** — a control plane *on top of* **vanilla** buildkit/containerd, no fork,
  no custom snapshotter → auditable, durable, no proprietary black box.
- **First-class multi-tenant security** — repo-bound OIDC (a caller can only ever build *its own*
  repo), ephemeral read-only fork daemons with no write-back (anti cache-poisoning), network
  policies, and **kernel-level isolation via Kata microVM** for untrusted fork PRs. Most SaaS in the
  segment do not offer this level of isolation for untrusted code.
- **Cost** — no metered build-minutes nor cache GB/month; cost = the **underlying Kubernetes
  infrastructure** you already operate.

---

## 5. Scope

buildkit-operator is a **build engine + control plane** you self-host, not a hosted product. The
rows marked ❌ in the table above — built-in billing/metering, a self-serve web dashboard, per-tenant
quotas, an integrated registry — are conveniences of a multi-tenant SaaS, not of the build engine
itself. They are deliberately out of scope: for an **internal / sovereign platform**, identity,
isolation, routing and lifecycle are handled in-cluster, and usage is observed through Prometheus
([operations.md](operations.md)) rather than a billing portal.

---

## 6. Sources

- [Depot — overview](https://depot.dev/docs/container-builds/overview) ·
  [products](https://depot.dev/products/container-builds) ·
  [changelog](https://depot.dev/changelog) ·
  [YC](https://www.ycombinator.com/companies/depot)
- [Docker Build Cloud](https://www.docker.com/products/build-cloud/)
- [Earthly — "A message about Earthly" (Satellites shutdown)](https://earthly.dev/blog/shutting-down-earthfiles-cloud/) ·
  [self-hosted satellites](https://docs.earthly.dev/earthly-cloud/satellites/self-hosted)
- [Blacksmith — remote BuildKit](https://www.blacksmith.sh/blog/faster-docker-builds-using-a-remote-buildkit-instance) ·
  [WarpBuild — remote Docker builders](https://www.warpbuild.com/products/ci/docker-builders) ·
  [Blacksmith vs WarpBuild](https://www.warpbuild.com/blog/blacksmith-warpbuild-comparison)
- [Namespace alternatives (comparison)](https://betterstack.com/community/comparisons/namespace-alternatives/) ·
  [GitHub Actions Runner Showdown 2026](https://tenki.cloud/blog/github-actions-runner-showdown-2026) ·
  [awesome-github-actions-runners](https://github.com/neysofu/awesome-github-actions-runners)
- Adjacent segment: [BuildBuddy](https://github.com/buildbuddy-io/buildbuddy) ·
  [EngFlow remote caching](https://www.engflow.com/product/remoteCaching) ·
  [Nx remote cache](https://nx.dev/remote-cache) ·
  [Dagger](https://github.com/dagger/dagger)
