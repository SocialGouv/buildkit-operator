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
  `newuidmap: Could not set caps`. **Fix**: add the daemon namespace to the policy's `exclude` list
  (alongside the pre-excluded `kube-system`, `prometheus-operator`).
  - **`PolicyException` is disabled cluster-wide** ("PolicyException resources would not be processed
    until it is enabled") — so a namespace exclusion on the policy itself is the only lever.
  - A live `kubectl patch` of the policy is config drift — track the exclusion in GitOps.
  - A container that *explicitly* sets `privileged: true` (e.g. a Kata fork daemon, or the kata-deploy
    installer) is not blocked by these policies; `kube-system` is exempt, so privileged installers run
    there cleanly.
- `disallow-host-path` (Audit), `disallow-host-ipc-ipd-network` (Enforce), `presence-of-securitycontext`.
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

## Sandboxed (Kata) runtime on MKS

- Install Kata with `kata-deploy` (Helm), `node-feature-discovery.enabled=false`, scoped to the build
  nodepool, tolerating its taint. It reconfigures and restarts containerd on the node, but **running
  pods survive the restart** (≈ zero downtime), so it can be rolled onto a node already serving work.
- Use **`kata-clh`** (cloud-hypervisor), not `kata-qemu`, and tune the guest to **≥ 4 vCPUs** — under
  nested virt a slow/under-provisioned guest misses containerd's CRI `get state` deadline and the VM is
  killed. Full rationale + setup: [sandboxed-builds.md](sandboxed-builds.md) and
  [../deploy/kata/](../deploy/kata/).

## Two clusters / one kubeconfig

The platform kubeconfig carries both a dev and a prod context. Pin `--context` on every command, and
treat prod as read-only unless a change is explicitly authorised for a specific action.
