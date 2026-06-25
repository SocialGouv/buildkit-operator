# buildkit-operator — documentation

Deep-dive docs that complement the top-level [README](../README.md). The root README is the
overview (why, architecture sketch, features, install); these documents are the reference for the
**design rationale, the security model, and the measured evidence** from validating buildkit-operator
on a real OVH Managed Kubernetes cluster (GRA9, Cinder gen2).

| Doc | What it answers |
|---|---|
| [architecture.md](architecture.md) | How the control plane routes and manages daemons: the cache-sharing routing key, the reconcile loop, lifecycle states, control-plane HA, the shared SNI gateway for off-cluster CI. |
| [security.md](security.md) | Why rootless `buildkitd` needs `no_new_privs` off, the Kyverno/PSS constraint and its fix, the threat model, and what per-project + fork isolation actually buy you. |
| [storage-and-cold-cache.md](storage-and-cold-cache.md) | The three storage layers (warm PVC, durable VolumeSnapshots, **cold S3**), how the S3 cache is wired, and what it measurably buys (cold ≈ 9×). |
| [performance.md](performance.md) | Methodology + measured build times (warm vs cold, with vs without S3). |
| [comparison-buildkit-service.md](comparison-buildkit-service.md) | Side-by-side with the existing shared `buildkit-service` (architecture, security posture, performance, durability). |
| [ci-integration.md](ci-integration.md) | The GitHub Action and the CI-agnostic core (GitHub/GitLab/any runner), the public exposure (LB + mTLS + cert SANs), and the example repo. |
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
- **Integration is CI-agnostic** — the GitHub Action wraps a single `build.sh` (route → `buildx
  remote` over mTLS) that works on a stock GitHub-hosted runner and on GitLab unchanged.
