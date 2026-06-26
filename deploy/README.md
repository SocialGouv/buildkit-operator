# buildkit-operator — deployment

This directory ships the **cluster-wide install** of buildkit-operator: the `buildd`
control plane, its RBAC, the generated CRDs, the shared mTLS certs, the shared
`buildkitd.toml` GC config, and the snapshot-class reference.

It does **not** contain any per-project buildkitd resources. `buildd` reconciles
each `BuildProject` into **one** StatefulSet-of-1 vanilla `buildkitd` per
`(project, arch)`, with its own Service (TCP+mTLS :1234) and a Cinder gen2 cache
PVC — all created by the controller at runtime.

```
deploy/
  config/buildkitd.toml         # GC config mounted into every per-project daemon
  cert/create-certs.sh          # mints the shared mTLS material (wildcard daemon SAN)
  cert/.certs/                  # generated certs + Secret manifests (gitignored)
  crd/                          # `task manifests` writes the generated CRDs here
  rbac/                         # `task manifests` writes generated RBAC here (reference)
  helm/buildkit-operator/                # the Helm chart for the control plane
```

## Prerequisites

- An OVH Managed Kubernetes cluster (the target) with:
  - StorageClass **`csi-cinder-high-speed-gen2`** (gen2 high-speed Cinder).
  - VolumeSnapshotClass **`csi-cinder-snapclass-v1`**.
  Both ship with OVH MKS; this chart only **references** them, it does not create
  them. Verify:
  ```bash
  kubectl get storageclass csi-cinder-high-speed-gen2
  kubectl get volumesnapshotclass csi-cinder-snapclass-v1
  ```
- `helm` 3.x, `kubectl`, and either `mkcert` or `openssl` on the box you run the
  cert script from.

## Install order

### (a) Generate the CRDs

```bash
task manifests
```

This runs `controller-gen` and writes the CRDs to `deploy/crd/`. To have Helm
install them with the chart, also copy them into the chart's `crds/` directory
(Helm installs CRDs from there once, and never templates/upgrades them):

```bash
cp deploy/crd/*.yaml deploy/helm/buildkit-operator/crds/
```

(Alternatively apply them out of band with `kubectl apply -f deploy/crd` and skip
the copy.)

### (b) Mint and apply the mTLS certs

Client ↔ daemon traffic is mutually authenticated. One daemon server cert with a **wildcard SAN**
(`*.<builds-ns>.svc`, `*.<builds-ns>.svc.cluster.local`, `localhost`, `127.0.0.1`) covers every
per-project Service the controller creates. The certs go in the **builds** namespace
(`buildkit-builds`) — that is where the daemons mount them:

```bash
deploy/cert/create-certs.sh buildkit-builds          # the builds namespace (daemons live here)
kubectl -n buildkit-builds apply -f deploy/cert/.certs/buildkit-daemon-certs.yaml
kubectl -n buildkit-builds apply -f deploy/cert/.certs/buildkit-client-certs.yaml
```

This produces two Secrets in the **builds** namespace:
- `buildkit-daemon-certs` (`ca.pem`/`cert.pem`/`key.pem`) — mounted by every
  per-project buildkitd daemon.
- `buildkit-client-certs` (`ca.pem`/`cert.pem`/`key.pem`) — distributed to CI runners
  (the GitHub Action / `build` CLI) so they can mTLS-dial the daemons.

The script prefers `mkcert`, falls back to `openssl`, is idempotent (an existing
CA is reused so already-deployed client certs stay valid), and never touches your
system trust store. The generated material is gitignored.

### (c) Install the chart

```bash
helm install buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator --create-namespace
```

This installs the `buildd` Deployment + Service (HTTP `/route` API and
`/healthz` on :8080), the ServiceAccount, the least-privilege ClusterRole +
binding, and the `buildkitd.toml` ConfigMap. Watch it roll out:

```bash
kubectl -n buildkit-operator rollout status deploy/buildkit-operator-buildd
```

Then create a `BuildProject` and watch the controller materialise its daemon (in the **builds**
namespace, where the chart placed the certs/config):

```bash
kubectl -n buildkit-builds get buildprojects -w
kubectl -n buildkit-builds get statefulset,svc,pvc -l app.kubernetes.io/name=buildkit-operator
```

### (d) OVH gen2 storage / snapshot classes

`csi-cinder-high-speed-gen2` (StorageClass) and `csi-cinder-snapclass-in-use-v1` (VolumeSnapshotClass)
are expected to **already exist** on the OVH cluster. The **snapshot** class is a chart value
(`snapshotClassName`); the **storage** class is a per-project field (`BuildProject.spec.storageClass`,
CRD-defaulted to `csi-cinder-high-speed-gen2`) — not a chart-wide value. If your cluster names them
differently:

```bash
# snapshot class (chart-wide):
helm install buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator --create-namespace \
  --set snapshotClassName=<your-vsc>
# storage class is set per BuildProject (or change the CRD default).
```

## Security profile & the Kyverno caveat

The per-project daemons default to **rootless** buildkit
(`securityProfile: rootless`, image `moby/buildkit:v0.31.1-rootless`). Rootless
buildkit needs `seccompProfile: Unconfined` on the daemon pod (it manages its own
user-namespaced sandbox).

A restrictive admission policy — e.g. a Kyverno baseline that *forces*
`allowPrivilegeEscalation: false` or rejects `seccompProfile: Unconfined` — blocks the daemons. The
daemons run in the **`buildkit-builds`** namespace, so that is the namespace to exempt. Two ways out:

1. **Namespace exemption (preferred):** exempt `buildkit-builds` from the offending policy. On a
   platform that owns its ClusterPolicies via GitOps, add `buildkit-builds` to the policy's
   `excludedNamespaces` (the control-plane namespace `buildkit-operator` needs **no** exemption — it is
   fully hardened). Full matrix in [docs/security.md](../docs/security.md) and
   [docs/operations.md](../docs/operations.md#kyverno-exemption). Where `PolicyException` is enabled you
   can instead scope an exception to the buildkitd pods in `buildkit-builds`.

2. **Fallback security profile:** the profile is **per-project**
   (`BuildProject.spec.securityProfile`, CRD-defaulted to `rootless`) — not a chart-wide value. Set it
   on the `BuildProject` (or change the CRD default) to `userns` or `privileged` so `Unconfined` is no
   longer required:

   - `userns` needs the `UserNamespacesSupport` feature gate on kubelet + kube-apiserver and uses
     `moby/buildkit:<ver>` (non-rootless) with `hostUsers: false`.
   - `privileged` runs the daemon privileged — only if nothing else is permitted. (Untrusted forks are
     better isolated with the Kata sandbox runtime — see [docs/sandboxed-builds.md](../docs/sandboxed-builds.md).)

## Uninstall

```bash
helm uninstall buildkit-operator -n buildkit-operator
# CRDs are intentionally NOT removed by Helm; delete them explicitly if desired:
kubectl delete -f deploy/crd --ignore-not-found
```
