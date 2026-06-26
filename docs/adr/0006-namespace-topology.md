# 0006 — Three-namespace topology, split by trust/role

**Status:** Accepted · **Date:** 2026-06-26

## Context

buildkit-operator has workloads in three distinct trust/privilege classes, and the OVH platform
enforces admission policy (Kyverno) per **namespace**:

- **control plane** (buildd + gateway) — hardened (nonroot, readOnlyRootFS, caps drop ALL); needs **no**
  privileged exemption;
- **build daemons** (per-project buildkitd + untrusted forks) — run untrusted code; rootless relaxes
  `allowPrivilegeEscalation` + seccomp/apparmor, so they need the **`securityContextPolicy`**
  (`add-custom-mas-securitycontext`) exemption;
- **Kata node plumbing** (`kata-deploy` + `kata-clh-vcpu-tune`) — privileged + hostPath, root on the
  node; needs the **`disallow-host-path`** exemption.

Originally everything operator-side lived in **one** namespace (`buildkit-operator`). That forces the
*control plane* to share a namespace that carries the security-context exemption it doesn't need, and
mixes "runs untrusted builds" with "runs the trusted operator" in one blast radius. The Kata plumbing's
upstream default is `kube-system`, which is exempt-by-default — convenient, but dumps a high-privilege
third-party installer into the platform's most powerful shared namespace.

## Decision

Split into **three** namespaces, each carrying **only** the exemption its workloads need (least
privilege):

| Namespace | Workloads | Kyverno exemption |
|---|---|---|
| `buildkit-operator` | control plane: buildd + gateway | **none** |
| `buildkit-builds` | per-project build daemons (+ untrusted forks), their certs/config/mirror, the `BuildProject` CRs | `securityContextPolicy` |
| `buildkit-system` | Kata node plumbing (kata-deploy + vcpu-tune) | `securityContextPolicy` + `disallow-host-path` |

The chart renders the `buildkit-operator` + `buildkit-builds` namespaces (`createNamespaces`) and
places each resource accordingly via the `operatorNamespace`/`buildsNamespace` helpers; `buildkit-system`
is created with the Kata install (out-of-band). buildd creates daemons in `--namespace`
(`buildkit-builds`) but takes its leader-election Lease in the namespace it **runs in** (the operator
ns, via the `POD_NAMESPACE` downward API) — the one code change the split requires.

## Alternatives considered

- **One namespace for everything (status quo).** Rejected: the control plane inherits an exemption it
  doesn't need, and untrusted builds share a blast radius with the operator.
- **Two namespaces** (`buildkit-system` for Kata + `buildkit-operator` for control plane *and* daemons).
  Rejected: still leaves the control plane in an exempt namespace; the daemons are the part that needs
  the relaxation, not buildd.
- **Kata in `kube-system`** (upstream default; exempt for free). Rejected deliberately: it avoids one
  manual Kyverno exemption but puts a privileged root-on-node installer into the platform's most
  trusted shared namespace, opaque to ownership/audit on a multi-tenant cluster. A dedicated
  `buildkit-system` is cleaner; the cost is adding it to `disallow-host-path` explicitly.
- **Daemons in `buildkit-operator` (do NOT add a builds ns).** Rejected: it's exactly the anti-pattern
  above — the operator namespace would carry the security-context exemption for the daemons' sake.

## Consequences

- ✅ Least privilege per namespace: the highest-value namespace (the operator) carries **no** exemption;
  the untrusted-execution namespace (`buildkit-builds`) is a clean, self-contained blast radius for
  NetworkPolicy / ResourceQuota / the `untrusted` label posture.
- ✅ Clear ownership/lifecycle separation: node capability (`buildkit-system`) vs control plane
  (`buildkit-operator`) vs runtime (`buildkit-builds`).
- ⚠️ Manual Kyverno exemptions instead of relying on `kube-system`'s freebie (lists **replace**, not
  merge, per cluster):
  - `buildkit-builds` → `securityContextPolicy.excludedNamespaces` (append in `apps-infra`
    `kyverno/ovh-prod.values.yaml`);
  - `buildkit-system` → **both** `securityContextPolicy` *and* `disallowHostPath` (like `kube-system`):
    the privileged installer is invalid once `allowPrivilegeEscalation` is mutated to false. Add it to
    `securityContextPolicy` in `ovh-prod.values.yaml` and to `disallowHostPath` in `common.values.yaml`
    (so prod doesn't drop the other host-path exemptions).
- ⚠️ More objects to operate (extra namespaces, cross-namespace references, the `POD_NAMESPACE`
  plumbing) and a chart that targets two namespaces from one release. Accepted for the isolation win.
- ⚠️ Migrating an existing single-namespace install means recreating the daemons + certs in
  `buildkit-builds` — a deliberate, disruptive migration (see [operations.md](../operations.md)).
