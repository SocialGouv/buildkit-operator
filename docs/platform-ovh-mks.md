# Platform notes — OVH Managed Kubernetes (MKS)

Durable, platform-specific facts for running buildkit-operator on OVH MKS. These are constraints of
the managed platform, not of the operator.

## Nodes

- Ubuntu 22.04, **containerd 1.7.x**, kernel **5.15**.
- General-purpose b2 flavours (e.g. 16 vCPU / 60 GiB). The Kubernetes `node.kubernetes.io/instance-type`
  label is an opaque UUID — the flavour name is not exposed via the API.
- **Nested virtualization is available** on b2 nodes (`/dev/kvm`, CPU `vmx`, `kvm_intel nested = Y`) — so
  microVM runtimes (Kata) can run. See [sandboxed-builds.md](sandboxed-builds.md).
- **Managed nodes are recycled.** Anything node-level (a runtime install, a config tweak) must be driven
  by a DaemonSet/operator keyed on a node label or the nodepool, so replacement nodes are covered
  automatically — never a one-off manual edit.
- No SSH. To read a node's journal: run a privileged `hostPID` pod in `kube-system` and
  `chroot /host journalctl …`.

## Kyverno admission policies

The platform ships cluster-wide Kyverno `ClusterPolicy` objects that materially affect build daemons:

- **`add-custom-mas-securitycontext`** *mutates* every container's securityContext to
  `allowPrivilegeEscalation: false` (+ `runAsNonRoot: true`, drop `NET_RAW`). This **breaks rootless
  buildkit**: rootlesskit's setuid `newuidmap` needs `no_new_privs` OFF, and fails with
  `newuidmap: Could not set caps`. It also makes a **privileged** container invalid (privileged +
  `allowPrivilegeEscalation:false` is rejected). **Fix**: add the relevant namespace to the policy's
  `exclude`/`excludedNamespaces` list (the list **replaces**, not merges).
  - **`PolicyException` is disabled cluster-wide** ("PolicyException resources would not be processed
    until it is enabled") — so a namespace exclusion on the policy itself is the only lever.
  - A live `kubectl patch` of the policy is config drift — track the exclusion in GitOps (`apps-infra`).
- `disallow-host-path` (Audit), `disallow-host-ipc-ipd-network` (Enforce), `presence-of-securitycontext`.
- **buildkit-operator's three namespaces each need a different exemption** (least privilege — see
  [ADR 0006](adr/0006-namespace-topology.md)):

  | Namespace | Workloads | Exempt from |
  |---|---|---|
  | `buildkit-operator` | control plane (buildd, gateway), hardened | — (nothing) |
  | `buildkit-builds` | per-project daemons + privileged Kata forks | `securityContextPolicy` |
  | `buildkit-system` | Kata node plumbing (privileged + hostPath) | `securityContextPolicy` **and** `disallow-host-path` (like `kube-system`) |
- **Secret-sync policies** generate shared secrets into *every* namespace (observed: a
  `buildkit-client-certs`, plus wildcard/registry secrets). This **collides** with the chart's default
  cert secret names → use product-prefixed names (`certs.daemonSecretName` / `certs.clientSecretName`).

## Storage

- `csi-cinder-high-speed-gen2` — gen2 Cinder; on gen2 **throughput scales with volume size**, so size
  cache volumes for bandwidth, not just capacity. `volumeBindingMode: Immediate`.
- VolumeSnapshot classes `csi-cinder-snapclass-v1` (at-rest) and `csi-cinder-snapclass-in-use-v1`
  (the in-use variant lets you snapshot a hot daemon without scale-to-zero).
- Cinder block PVCs attach fine inside Kata microVMs.

## Networking & images

- CNI is Canal/Calico — **NetworkPolicy is enforced** (the daemon egress lockdown works).
- The cluster pulls **public** GHCR images anonymously; there is no private-pull credential wired in.
  Operator images must be public. Note: GHCR container-package visibility cannot be flipped via the REST
  API (404) — it is a UI/manual action.

## LoadBalancer idle-timeout

OVH MKS LoadBalancers are OpenStack/Octavia, where the default **member-data idle timeout is 50 s**.
That cuts two buildkit-operator paths once buildd/gateway are exposed:

- a **cold** `/route` blocks while a daemon is provisioned (buildd waits up to `--route-wait`, 180 s) —
  the LB cuts it at 50 s and the client sees `curl: (52) Empty reply from server` on the first build;
- a build holds **one long-lived mTLS stream** through the gateway; a quiet stretch (a long `RUN` with
  no output) is cut the same way.

The chart raises both via Service annotations the OpenStack CCM honors — buildd
`loadbalancer.openstack.org/timeout-{member,client}-data: "200000"` (> route-wait), gateway `"600000"`.
The CCM updates the Octavia listener **in place** (no LB recreation, the external IP is preserved).

## Sandboxed (Kata) runtime on MKS

- Install Kata with `kata-deploy` (Helm), `node-feature-discovery.enabled=false`, scoped to the build
  nodepool, tolerating its taint, into the dedicated **`buildkit-system`** namespace (exempt it from
  `securityContextPolicy` + `disallow-host-path`, like `kube-system`).
  - ⚠️ It installs `/opt/kata` (hostPath) and **reconfigures + RESTARTS containerd** on the node to
    register the runtime handlers. **Running pods survive** the restart (it doesn't kill containers),
    but the node's CRI control-plane blips — do it in a **maintenance window**, on the **dedicated
    build nodepool only**. `kata-clh-vcpu-tune` does **not** restart containerd (per-sandbox config).
- Use **`kata-clh`** (cloud-hypervisor), not `kata-qemu`, and tune the guest to **≥ 4 vCPUs** — under
  nested virt a slow/under-provisioned guest misses containerd's CRI `get state` deadline and the VM is
  killed. Full rationale + setup: [sandboxed-builds.md](sandboxed-builds.md) and
  [../deploy/kata/](../deploy/kata/).

## Two clusters / one kubeconfig

The platform kubeconfig carries both a dev and a prod context. Pin `--context` on every command, and
treat prod as read-only unless a change is explicitly authorised for a specific action.
