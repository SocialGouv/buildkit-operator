# 0005 — Kata (cloud-hypervisor) microVMs for untrusted fork isolation

**Status:** Accepted · **Date:** 2026-06-26 (backfilled)

## Context

Every `RUN` step runs attacker-controlled code. For **trusted** branches, rootless buildkit + a
network lockdown is acceptable (the daemon shares the host kernel but is unprivileged). For
**untrusted** code — fork / external PRs — a shared kernel is a real escape surface: a kernel exploit
in a `RUN` step breaks out to the node, which on a shared OVH MKS cluster means other teams' workloads.

We already isolate forks at the cache layer (ephemeral `ForkKey` daemon, read-only snapshot seed, no
write-back — [0001](0001-control-plane-over-vanilla-buildkit.md)) and optionally at the network layer
(`forkEgressStrict`). The missing layer is **kernel/VM isolation** for the untrusted build itself.

## Decision

Run **untrusted fork daemons inside a Kata Containers microVM** using the **cloud-hypervisor**
runtime (`kata-clh`). Inside the disposable VM, buildkit runs **non-rootless + privileged** — the VM,
not the container, is the security boundary, and rootless's setuid `newuidmap` can't run in the guest
anyway. Trusted/canonical daemons are **unchanged** (rootless under runc, for full speed). It is
opt-in via `sandbox.runtimeClass: kata-clh`, applied to fork daemons only.

Kata is installed by the upstream **`kata-deploy`** chart, scoped to a **dedicated build nodepool**
(`nodeSelector: { nodepool: prod-build }`) so replacement nodes auto-get it, and into a **dedicated
`buildkit-system` namespace** (see [0006](0006-namespace-topology.md)).

## Alternatives considered

| Option | Why rejected |
|---|---|
| **runc-rootless only** (status quo for trusted) | Shared host kernel — insufficient isolation for untrusted code. |
| **Sysbox** (userns, keeps `no_new_privs`) | Stronger than runc but still a **shared kernel**; and the CE installer **refuses recent Kubernetes** (e.g. v1.31, "EOL") and is effectively unmaintained. |
| **gVisor (runsc)** (user-space kernel) | **Breaks** buildkit's nested executor (runc + overlayfs inside the sandbox) — builds don't run. |
| **kata-qemu** | Under **nested** virt qemu boots too slowly; the kata-agent misses containerd's CRI `get state` deadline (`context deadline exceeded`) and the kubelet kills the sandbox. |
| **Kata + cloud-hypervisor (chosen)** | Real microVM (own guest kernel) so buildkit's nested `RUN` works; clh boots fast enough under nested virt. **Requires** a ≥4-vCPU guest floor (1 vCPU → agent too slow → VM restart-loops). |

Isolation ranking: Kata (microVM) **>** Sysbox (shared-kernel userns) **>** runc-rootless. So Kata is
not merely "as good as" the abandoned Sysbox — it is **stronger**; the trade-off is overhead
(VM boot, cold start, the vCPU floor), not security.

## Consequences

- ✅ A breakout in an untrusted build is confined to a throw-away VM, not the node — meaningful on a
  shared cluster. Proven on OVH MKS (guest kernel 6.18 vs host 5.15, real root `RUN` build, stable).
- ⚠️ **Node-level install, with a containerd restart.** `kata-deploy` installs binaries to `/opt/kata`
  (hostPath) and **reconfigures + RESTARTS containerd** on each pool node to register the runtime
  handlers. A containerd restart does **not** kill running containers, but the node's CRI control-plane
  blips — do it in a **maintenance window**, scoped to the **dedicated build nodepool only** (never
  shared nodes). The incumbent `buildkit-service` shares `prod-build` and survives the restart, but the
  action is deliberate. (`kata-clh-vcpu-tune` patches the vCPU floor **without** a restart — Kata reads
  config per-sandbox.) Full warning + Kyverno steps in [deploy/kata/README.md](../../deploy/kata/README.md).
- ⚠️ Needs a **dedicated nodepool with nested virt** (`/dev/kvm`, `vmx`/`svm`); not viable on shared
  nodes (node-level ops forbidden there) — which is the same reason Sysbox couldn't be installed on the
  shared pool. This was the structural constraint, independent of runtime choice.
- ⚠️ Cold start slower than runc (VM boot + first pull), and many microVMs at once can cascade into
  kill-loops on a busy node — bounded by `--max-cold-starts`. Fine for non-latency-critical PR builds.
- ⚠️ S3 cold-cache credentials are env vars on the daemon; an untrusted build could read `/proc/1/environ`
  — so untrusted forks with S3 enabled **must** run under Kata (the microVM hides host `/proc`). See
  [security.md](../security.md).
