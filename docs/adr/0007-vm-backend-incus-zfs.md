# 0007 — A pluggable provisioning backend; single-host Incus + ZFS alongside Kubernetes

**Status:** Proposed · **Date:** 2026-06-29

## Context

buildkit-operator was built as a Kubernetes control plane ([0001](0001-control-plane-over-vanilla-buildkit.md)):
one hot vanilla `buildkitd` per `(project, arch)` materialised as a StatefulSet-of-1 + Service + retained
Cinder PVC, reconciled from a `BuildProject` CRD. That is the right shape on OVH Managed Kubernetes.

But the *value* is the control plane — routing to the daemon that shares a cache, and managing that
daemon's lifecycle — not Kubernetes itself. There are deployments where a cluster is overkill or
unavailable: a single beefy build VM, a developer host, an on-prem/air-gapped box. We want the same
warm-cache-per-project behaviour there, without running Kubernetes.

Crucially, the codebase was already ~80 % substrate-agnostic. The router ([`internal/router`](../../internal/router))
is a pure function; the client (`cmd/build`) and the CI Actions only ever `POST /route` then
`docker buildx --driver remote` against an mTLS `host:port`; OIDC identity is backend-neutral. Only one
layer was bound to Kubernetes: the *provisioner* — the code that turns a key into a running, addressable
daemon and manages its lifecycle.

## Decision

Introduce a small, imperative **`provisioner.Provisioner` interface**
([`internal/provisioner`](../../internal/provisioner)) — `Ensure / Ready / WaitReady / Endpoint /
AddInflight` — that the routing API (`cmd/buildd`) depends on instead of a controller-runtime client.

- **Kubernetes stays the default backend** ([`internal/provisioner/k8s`](../../internal/provisioner/k8s)):
  the existing ensure/ready/wait/endpoint/inflight + fork-derivation logic moved **verbatim** behind the
  interface. The reconcile/scale/snapshot loop stays wired as controller-runtime manager Runnables in
  buildd's k8s setup — the k8s path is behaviourally unchanged.
- **Add a single-host backend** ([`internal/provisioner/local`](../../internal/provisioner/local)),
  selected by `--backend local`: one **Incus instance** running buildkitd per project, backed by a
  retained **ZFS dataset** (the warm cache), with an in-process scale-to-zero reconcile loop instead of a
  controller. State (inflight / last-build) is in-memory — a single process, no CRD, no etcd.

The lifecycle *wiring* genuinely differs per backend (a controller-runtime manager vs a goroutine), so
it lives in buildd's per-backend setup, not behind the interface. The interface is only the imperative
surface the handlers call.

Why Incus + ZFS (and not Docker): the k8s feature set maps almost 1:1 onto Incus + ZFS, often more
simply — PVC → ZFS dataset; VolumeSnapshot → `zfs snapshot`; CoW fan-out → `zfs clone`; Kata microVM →
Incus VM instance; scale-to-zero → `incus stop/start`. Docker has none of snapshot/CoW/VM-isolation
natively, so it cannot reach full parity.

## Alternatives considered

- **Docker (or systemd-nspawn) backend.** Rejected for the full-parity goal: no native durable snapshots,
  no CoW clone for fork seeding, no VM isolation for untrusted forks. Incus gives all three.
- **Fork the repo for a non-k8s variant.** Rejected: forfeits shared maintenance and duplicates the
  routing/identity/client code with no clean seam.
- **Keep k8s-only.** Rejected: excludes the single-VM / on-prem / dev audience for whom a cluster is the
  dominant cost.

## Consequences

- ✅ Same client, Actions, router and OIDC across both backends — only `/route`'s provisioner differs.
- ✅ Single-VM deployments with no cluster; cold start off local ZFS avoids the Cinder network-attach
  latency that dominates today ([0003](0003-scale-to-zero-retained-pvc.md)); fork isolation is *native*
  (Incus VM) rather than requiring Kata on a dedicated nodepool ([0005](0005-kata-clh-for-untrusted-forks.md)).
- ⚠️ No horizontal scale across nodes and no HA on the local backend — acceptable, and inherent, for a
  single-host target.
- ⚠️ A second backend to maintain + a doubled e2e matrix; egress hardening
  ([0006](0006-namespace-topology.md)'s NetworkPolicy) is re-implemented by binding an Incus network ACL
  per instance (strict for untrusted forks).
- ⚠️ **Status is Proposed**: the local backend is implemented — warm build + cache, scale-to-zero,
  durable ZFS snapshots with retention, CoW fork seeding (`zfs clone`) into a VM-isolated instance
  (untrusted builds are refused without a VM image), and a fan-out clone primitive — all covered by unit
  tests over a stubbed host seam. What remains before Accepted is **real end-to-end validation** on a
  host with Incus + a ZFS pool (plus an automatic saturation trigger for fan-out).
