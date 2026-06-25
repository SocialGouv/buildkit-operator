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

1. **Pick the node(s)** that will run sandboxed builds and label them (must expose nested virt —
   `/dev/kvm`, `vmx`/`svm`):

   ```sh
   kubectl label node <node> kata-node=true
   ```

2. **Install Kata** with the upstream `kata-deploy` Helm chart (kata-containers v3.32.0,
   `tools/packaging/kata-deploy/helm-chart/kata-deploy`), scoped to those nodes:

   ```sh
   helm dependency update <chart>
   helm install kata-deploy <chart> -n kube-system -f deploy/kata/kata-deploy-values.yaml
   ```

   It reconfigures containerd and **restarts it** on the labelled node (running pods survive the
   restart, but plan a short maintenance window if the node already runs workloads). When done it
   labels the node `katacontainers.io/kata-runtime=true` and creates the `kata-clh` RuntimeClass.

3. **Apply the vCPU tuning** (keeps the guest at 4 vCPUs / 4 GiB):

   ```sh
   kubectl apply -f deploy/kata/kata-clh-vcpu-tune.yaml
   ```

4. **Enable it in the operator** — set `sandbox.runtimeClass: kata-clh` in the chart values. The
   operator then renders fork daemons as the non-rootless buildkit image, `privileged`, with the
   `kata-clh` RuntimeClass (which pins them to the kata node), and skips the companion sidecar.

## Kyverno (OVH platform)

The `kata-deploy` installer and the tuning DaemonSet are privileged + hostPath; they run in
`kube-system`, which is already excluded from the platform's host-access policies — no extra exemption
needed. (The operator's daemon namespace still needs the `add-custom-mas-securitycontext` exclusion for
rootless canonical daemons; see ../README.md.)
