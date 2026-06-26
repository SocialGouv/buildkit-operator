# Architecture Decision Records

Short, durable records of the **load-bearing** decisions behind buildkit-operator: the context they
were made in, the options weighed, and the consequences accepted. They explain *why the project is
shaped the way it is* — the prose docs (architecture, security, sandboxed-builds…) explain *how it
works today*.

These were **backfilled on 2026-06-26** from the existing design docs, benchmarks and `.plans/`; the
decisions themselves were taken earlier during the phase-0 / M1–M5 build-out. Format is a trimmed
[MADR](https://adr.github.io/madr/): Status · Context · Decision · Alternatives · Consequences.

| # | Decision | Status |
|---|---|---|
| [0001](0001-control-plane-over-vanilla-buildkit.md) | Control plane over **vanilla** buildkit — one hot daemon per `(project, arch)` (no fork) | Accepted |
| [0002](0002-reject-distributed-snapshotter.md) | **Reject** building a distributed containerd snapshotter + CAS | Accepted |
| [0003](0003-scale-to-zero-retained-pvc.md) | Scale-to-zero with a **retained** Cinder PVC (attach, not restore) | Accepted |
| [0004](0004-shared-sni-gateway.md) | One **shared SNI gateway** for off-cluster CI (not one LB per daemon) | Accepted |
| [0005](0005-kata-clh-for-untrusted-forks.md) | **Kata (cloud-hypervisor)** microVMs for untrusted fork isolation | Accepted |
| [0006](0006-namespace-topology.md) | **Three-namespace** topology split by trust/role (operator / builds / system) | Accepted |

New decision? Copy the shape of an existing file, bump the number, default Status to `Proposed`.
