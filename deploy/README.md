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
  crd/                          # `make manifests` writes the generated CRDs here
  rbac/                         # `make manifests` writes generated RBAC here (reference)
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
make manifests
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

`buildd` ↔ daemon and client ↔ daemon traffic is mutually authenticated. One
daemon server cert with a **wildcard SAN** (`*.<ns>.svc`,
`*.<ns>.svc.cluster.local`, `localhost`, `127.0.0.1`) covers every per-project
Service the controller will ever create in the namespace.

```bash
deploy/cert/create-certs.sh buildkit-operator       # <ns> defaults to "buildkit-operator"
kubectl apply -f deploy/cert/.certs/buildkit-daemon-certs.yaml
kubectl apply -f deploy/cert/.certs/buildkit-client-certs.yaml
```

This produces two Secrets in the target namespace:
- `buildkit-daemon-certs` (`ca.pem`/`cert.pem`/`key.pem`) — mounted by every
  per-project buildkitd daemon.
- `buildkit-client-certs` (`ca.pem`/`cert.pem`/`key.pem`) — mounted by `buildd`.

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

Then create a `BuildProject` and watch the controller materialise its daemon:

```bash
kubectl -n buildkit-operator get buildprojects -w
kubectl -n buildkit-operator get statefulset,svc,pvc -l app.kubernetes.io/name=buildkit-operator
```

### (d) OVH gen2 storage / snapshot classes

As noted in the prerequisites, `csi-cinder-high-speed-gen2` (StorageClass) and
`csi-cinder-snapclass-v1` (VolumeSnapshotClass) are expected to **already exist**
on the OVH cluster. They are the chart defaults
(`defaults.storageClass`, `snapshotClassName`). If your cluster names them
differently, override at install time:

```bash
helm install buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator --create-namespace \
  --set defaults.storageClass=<your-sc> \
  --set snapshotClassName=<your-vsc>
```

## Security profile & the Kyverno caveat

The per-project daemons default to **rootless** buildkit
(`securityProfile: rootless`, image `moby/buildkit:v0.18.2-rootless`). Rootless
buildkit needs `seccompProfile: Unconfined` on the daemon pod (it manages its own
user-namespaced sandbox).

A restrictive admission policy — e.g. a Kyverno `restrict-seccomp` /
Pod-Security-`restricted` baseline — will **reject** `seccompProfile: Unconfined`
and block the daemons from starting. Two ways out:

1. **PolicyException (preferred):** grant the buildkit-operator namespace an exception for
   the seccomp rule, scoped to the buildkitd pods. With Kyverno:

   ```yaml
   apiVersion: kyverno.io/v2
   kind: PolicyException
   metadata:
     name: buildkit-operator-rootless-seccomp
     namespace: buildkit-operator
   spec:
     exceptions:
       - policyName: restrict-seccomp          # your cluster's policy name
         ruleNames:
           - "*"
     match:
       any:
         - resources:
             kinds: ["Pod"]
             namespaces: ["buildkit-operator"]
             selector:
               matchLabels:
                 app.kubernetes.io/name: buildkit-operator
   ```

   (Adjust `policyName`/`ruleNames` to match the policy actually deployed on the
   cluster.)

2. **Fallback security profile:** if you cannot add an exception, switch the
   profile so `Unconfined` is no longer required:

   ```bash
   helm upgrade buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator \
     --set securityProfile=userns        # kubelet-assigned UID, UserNamespacesSupport
   # or, last resort:
   helm upgrade buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator \
     --set securityProfile=privileged
   ```

   - `userns` needs the `UserNamespacesSupport` feature gate on kubelet +
     kube-apiserver and uses `moby/buildkit:<ver>` (non-rootless) with
     `hostUsers: false`.
   - `privileged` runs the daemon privileged — only if nothing else is permitted.

## Uninstall

```bash
helm uninstall buildkit-operator -n buildkit-operator
# CRDs are intentionally NOT removed by Helm; delete them explicitly if desired:
kubectl delete -f deploy/crd --ignore-not-found
```
