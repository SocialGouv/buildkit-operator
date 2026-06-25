# buildkit-operator — documentation

Deep-dive docs that complement the top-level [README](../README.md). The root README is the
overview (why, architecture sketch, features, install); these documents are the reference for the
**design rationale, the security model, and the measured evidence** from validating buildkit-operator
on a real OVH Managed Kubernetes cluster (GRA9, Cinder gen2).

> **Status.** Deployed on **ovh-prod** via ArgoCD, coexisting with the shared `buildkit-service` on the
> build nodepool. Every capability below — per-project warm cache, scale-to-zero, S3 cold cache,
> VM-isolated untrusted forks (Kata/clh), and the supply chain (SLSA provenance + SBOM + cosign) — is
> exercised end to end on real OVH MKS, the last consumed over the public internet by the
> [example repo](https://github.com/SocialGouv/buildkit-operator-example).

| Doc | What it answers |
|---|---|
| [architecture.md](architecture.md) | How the control plane routes and manages daemons: the cache-sharing routing key, the reconcile loop, lifecycle states, control-plane HA, the shared SNI gateway for off-cluster CI. |
| [security.md](security.md) | Why rootless `buildkitd` needs `no_new_privs` off, the Kyverno/PSS constraint and its fix, the threat model, and what per-project + fork isolation actually buy you. |
| [sandboxed-builds.md](sandboxed-builds.md) | Running **untrusted fork** daemons in a disposable **microVM** (Kata): why Kata over Sysbox/gVisor, the cloud-hypervisor + ≥4-vCPU requirements, and how the operator wires privileged non-rootless buildkit into the VM. |
| [platform-ovh-mks.md](platform-ovh-mks.md) | Platform constraints on OVH Managed Kubernetes: the Kyverno mutation that breaks rootless and its fix, node recycling, nested virt, gen2 Cinder, public-image pull, and synced-secret collisions. |
| [storage-and-cold-cache.md](storage-and-cold-cache.md) | The three storage layers (warm PVC, durable VolumeSnapshots, **cold S3**), how the S3 cache is wired, and what it measurably buys (cold ≈ 9×). |
| [performance.md](performance.md) | Methodology + measured build times (warm vs cold, with vs without S3). |
| [comparison-buildkit-service.md](comparison-buildkit-service.md) | Side-by-side with the existing shared `buildkit-service` (architecture, security posture, performance, durability). |
| [ci-integration.md](ci-integration.md) | The GitHub Action and the CI-agnostic core (GitHub/GitLab/any runner), public exposure (LB + mTLS + cert SANs + **bearer-token auth** + the `gateway-ip` escape hatch), **supply-chain attestations** (SLSA/SBOM/cosign), and the example repo. |
| [benchmarks-phase0.md](benchmarks-phase0.md) | The Cinder gen2 storage benchmark that drives the config values (idle timeout, cold-start rate limit, fan-out viability). |
| [operations.md](operations.md) | Runbook: deploy, expose publicly, exempt Kyverno, run HA, observe, and tear down cleanly. |

## In short

- **The design holds in practice.** One vanilla `buildkitd` per `(project, arch)` shares layers
  *and* `RUN --mount=type=cache` mounts internally — no fork, no custom snapshotter.
- **The daemon security posture is incompressible and identical to the existing service** — rootless
  buildkit *requires* `no_new_privs` off + `Unconfined` profiles. buildkit-operator's security win is not a
  more-locked-down daemon; it is a **smaller blast radius** (per-project cache, fork isolation).
- **S3 is the cold-start answer the shared service lacks.** External, opt-in, daemon-side. Measured
  **≈ 9×** on a cold daemon (4.5 s vs 41.8 s), neutral when warm. See
  [storage-and-cold-cache.md](storage-and-cold-cache.md).
- **The control plane is HA** (leader election, 2 replicas; `/route` served by all).
- **Untrusted builds get a microVM.** Fork-PR daemons run privileged-but-non-rootless inside a
  Kata/cloud-hypervisor microVM (`sandbox.runtimeClass: kata-clh`) — proven on OVH MKS under nested
  virt. See [sandboxed-builds.md](sandboxed-builds.md).
- **Supply chain is daemon-side.** `push` + `provenance`/`sbom`/`sign` ⇒ SLSA provenance + SBOM +
  cosign keyless signature, produced by the daemon and verifiable by the caller — proven end to end.
- **Integration is CI-agnostic** — the GitHub Action wraps a single `build.sh` (route → `buildx
  remote` over mTLS) that works on a stock GitHub-hosted runner and on GitLab unchanged.
