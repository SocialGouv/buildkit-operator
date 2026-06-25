# GitOps integration

Three pieces are needed to put **VM-isolated untrusted builds** into service durably. They are live on
ovh-prod today (applied by hand); move them into your GitOps so they survive a cluster rebuild and node
recycling. Each is independent.

## 1. Kata on the build nodepool

The upstream `kata-deploy` chart has no published Helm repo, so reference its git repo. Scope it to the
build nodepool so every node — including MKS replacements — gets Kata automatically.

- Flux: [`kata-deploy.flux.yaml`](kata-deploy.flux.yaml) (GitRepository + HelmRelease).
- Argo CD: an `Application` with `source.repoURL=https://github.com/kata-containers/kata-containers`,
  `targetRevision=3.32.0`, `path=tools/packaging/kata-deploy/helm-chart/kata-deploy`, and the Helm values
  from [`../kata/kata-deploy-values.yaml`](../kata/kata-deploy-values.yaml).

Plus the **guest vCPU floor** (≥4 vCPUs — required under nested virt) as a plain DaemonSet, drop it in
as-is: [`../kata/kata-clh-vcpu-tune.yaml`](../kata/kata-clh-vcpu-tune.yaml).

## 2. Kyverno exclusion for the operator namespace

The platform's `add-custom-mas-securitycontext` ClusterPolicy mutates pods to
`allowPrivilegeEscalation: false`, which crashes **rootless** (canonical) daemons. Exclude the operator
namespace. Apply [`kyverno-exclude.patch.yaml`](kyverno-exclude.patch.yaml) wherever that ClusterPolicy
is managed (it is platform-owned, so this is a patch, not a full object). `PolicyException` is disabled
cluster-wide, so the policy's own `exclude` list is the only lever.

> Sandboxed *fork* daemons are privileged-in-a-VM and are **not** affected by this policy; only the
> rootless canonical daemons need the exclusion.

## 3. Enable the sandbox runtime in the operator

Add to the buildkit-operator Helm values (its own HelmRelease / Application):

```yaml
sandbox:
  runtimeClass: kata-clh
```

That is all the operator needs — it then renders untrusted fork daemons as the non-rootless buildkit
image, `privileged`, with `runtimeClassName: kata-clh` (pinned to the kata nodes), companion skipped.

---

Rationale and the full picture: [../../docs/sandboxed-builds.md](../../docs/sandboxed-builds.md) and
[../../docs/platform-ovh-mks.md](../../docs/platform-ovh-mks.md).
