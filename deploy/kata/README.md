# VM-isolated untrusted builds with Kata Containers

Untrusted fork daemons can run as **non-rootless, privileged buildkit inside a Kata microVM** instead
of rootless buildkit under runc. The microVM is the security boundary, so a malicious build is confined
to a disposable VM — stronger isolation than a shared kernel. Trusted (canonical) daemons are unaffected.

Proven on OVH MKS (nested virtualization): an operator-managed fork ran a real `RUN` build as root
inside the VM (guest kernel 6.18 vs host 5.15), stable.

## Why cloud-hypervisor + a vCPU floor

- Use the **`kata-clh`** RuntimeClass (cloud-hypervisor), **not** `kata-qemu`: under nested virt qemu
  boots too slowly, the kata-agent misses containerd's CRI `get state` deadline (`context deadline
  exceeded`) and the kubelet kills the sandbox.
- Even with clh, the guest needs **≥4 vCPUs**. With the default 1 vCPU the agent is still too slow and
  the VM is killed/looping every ~20–80s. `kata-clh-vcpu-tune.yaml` bumps `default_vcpus`/`default_memory`
  on every kata node and keeps them patched (Kata reads its config per-sandbox, so no containerd restart).

## Setup (reproducible)

1. **Target the build nodepool.** `kata-deploy-values.yaml` selects `nodeSelector: { nodepool: prod-build }`
   so every node in the pool — **including replacement nodes MKS recycles in** — gets Kata automatically
   (durable; survives a downscale). Set the label to match your build pool. The pool must expose nested
   virt (`/dev/kvm`, CPU `vmx`/`svm`).

2. **Install Kata** with the upstream `kata-deploy` Helm chart (kata-containers v3.32.0,
   `tools/packaging/kata-deploy/helm-chart/kata-deploy`), into the dedicated `buildkit-system`
   namespace:

   ```sh
   kubectl create namespace buildkit-system
   helm dependency update <chart>
   helm install kata-deploy <chart> -n buildkit-system -f deploy/kata/kata-deploy-values.yaml
   ```

   > ⚠️ **kata-deploy modifies the node.** On each targeted node it installs the Kata binaries under
   > `/opt/kata` (hostPath) and **reconfigures + RESTARTS containerd** to register the kata runtime
   > handlers. A containerd restart does **not** kill running containers, but the node's CRI
   > control-plane blips for a few seconds — schedule a short **maintenance window**. Keep this scoped
   > to the **dedicated build nodepool** (`kata-deploy-values.yaml` already does, via
   > `nodeSelector: { nodepool: prod-build }`); **never** run it against shared nodes that host other
   > teams' workloads. On `prod-build` the incumbent `buildkit-service` shares the pool — its daemons
   > survive the restart (a containerd restart leaves containers running), so the blip is tolerable, but
   > do it deliberately. When done it labels the node `katacontainers.io/kata-runtime=true` and creates
   > the `kata-clh` RuntimeClass.

3. **Apply the vCPU tuning** (keeps the guest at 4 vCPUs / 4 GiB). Unlike step 2 this does **not**
   restart containerd — Kata reads its config per-sandbox, so the next pod just picks up the new value:

   ```sh
   kubectl apply -f deploy/kata/kata-clh-vcpu-tune.yaml
   ```

   > ⚠️ **Deleting the `kata-deploy` DaemonSet tears the node down.** kata-deploy traps `SIGTERM` in
   > PID 1 and runs the node cleanup on pod termination (removes `/opt/kata`, reverts the containerd
   > config, drops the node label, restarts containerd). This is **not** a k8s `preStop` hook — the DS
   > `lifecycle` is empty — so a `kubectl delete ds` looks safe but fully de-configures Kata. To
   > **move** kata-deploy (e.g. between namespaces), treat it as a teardown + reinstall (a second
   > containerd reconfigure), in a maintenance window. **Uninstall via `helm uninstall`** — do not delete
   > the release secrets first, or its cluster-scoped objects (a ClusterRole, a ClusterRoleBinding, and
   > ~24 `RuntimeClass`es) are orphaned and the next `helm install` fails with `invalid ownership
   > metadata`. More gotchas in [../../docs/lessons-learned.md](../../docs/lessons-learned.md).

4. **Enable it in the operator** — set `sandbox.runtimeClass: kata-clh` in the chart values. The
   operator then renders fork daemons as the non-rootless buildkit image, `privileged`, with the
   `kata-clh` RuntimeClass (which pins them to the kata node), and skips the companion sidecar.

## Kyverno (OVH platform)

The `kata-deploy` installer and the tuning DaemonSet are **privileged + hostPath**, so `buildkit-system`
must be **explicitly exempted** — exactly like `kube-system` — from **both** host-access policies:
`disallow-host-path` (the hostPath mount) **and** `securityContextPolicy` /
`add-custom-mas-securitycontext` (whose `allowPrivilegeEscalation:false` mutation makes a *privileged*
container invalid). Unlike `kube-system`, `buildkit-system` is **not** exempt by default — this is the
deliberate cost of a clean, dedicated namespace instead of dumping node plumbing into `kube-system`.

In the platform Kyverno values (GitLab `apps-infra`). The lists **replace** (not merge) per cluster, so:

```yaml
# kyverno/ovh-prod.values.yaml  — this list REPLACES common.values.yaml, so include the existing entries
kyverno:
  securityContextPolicy:
    excludedNamespaces:
      - kube-system
      - prometheus-operator
      - buildkit-builds   # per-project daemons (rootless + privileged Kata forks)
      - buildkit-system   # Kata node plumbing (privileged installer)

# kyverno/common.values.yaml  — add buildkit-system here (harmless where the ns is absent), so the
# other host-path exemptions aren't dropped on prod
  disallowHostPath:
    excludedNamespaces:
      - kube-system
      - prometheus-operator
      - promtail
      - sealed-secrets-system
      - node-problem-detector
      - kured
      - buildkit-system   # Kata node plumbing: hostPath /opt/kata
```

The three buildkit-operator namespaces have **different** exemption needs (least privilege):

| Namespace | Workloads | Kyverno exemption |
|---|---|---|
| `buildkit-operator` | control plane (buildd + gateway), hardened | **none** |
| `buildkit-builds` | per-project daemons (rootless relaxes `allowPrivilegeEscalation`) + privileged Kata forks | `securityContextPolicy` |
| `buildkit-system` | Kata node plumbing (privileged + hostPath) | `securityContextPolicy` **and** `disallow-host-path` |

See [../../docs/operations.md](../../docs/operations.md) for the operator namespaces and
[../../docs/adr/0006-namespace-topology.md](../../docs/adr/0006-namespace-topology.md) for why they are split.
