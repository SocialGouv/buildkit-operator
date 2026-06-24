# Security model

This documents the security posture of buildcat, the **one hard constraint** rootless BuildKit
imposes (and why it is not negotiable), the **admission-policy friction** on a hardened platform and
its fix, and the threat-model improvements buildcat makes over a single shared daemon.

## The incompressible constraint: rootless `buildkitd` needs `no_new_privs` OFF

Rootless `buildkitd` sets up a user namespace with `newuidmap`/`newgidmap` (setuid helpers from
`shadow-utils`). Those helpers **must be able to gain the capabilities encoded in their file caps**,
which the kernel blocks when `no_new_privs` is set. Kubernetes sets `no_new_privs` whenever
`allowPrivilegeEscalation: false`. Therefore a rootless daemon requires:

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1000                 # NON-root
  allowPrivilegeEscalation:       # UNSET  (not false) — setting it false breaks newuidmap
  seccompProfile: { type: Unconfined }
  appArmorProfile: { type: Unconfined }   # without this: "failed to share mount point: permission denied"
```

Symptoms when this is wrong (both hit and fixed during bring-up):

- `allowPrivilegeEscalation: false` ⇒ `newuidmap: Could not set caps` ⇒ crash-loop.
- missing `appArmorProfile: Unconfined` ⇒ `failed to share mount point: permission denied`.

**This is a property of rootless BuildKit, not of buildcat.** Any rootless buildkit on Kubernetes —
including the existing `buildkit-service` — runs with exactly this posture. The pods remain
**non-root and unprivileged**; the only thing relaxed is `no_new_privs` (plus the default seccomp/
AppArmor filters, which the rootless engine manages itself). The alternatives are heavier, not
lighter: `securityProfile: userns` (host userns config) or `privileged` (a real privilege increase).

## Admission policy (Kyverno / restricted PSS)

The fabrique OVH platform ships a Kyverno `ClusterPolicy` (`add-custom-mas-securitycontext`) that
**mutates every pod** to `allowPrivilegeEscalation: false`. That silently breaks rootless buildkit
(see above). Two ways out:

1. **Exempt the daemon namespace** from the mutate rule — the precedented pattern on this platform
   (the `arc-runners` CI namespace is already exempted for the same reason). This is the recommended
   fix: the exemption is scoped to a dedicated build namespace, and the pods are still non-root.
2. Switch `securityProfile` to `userns`/`privileged` (worse — a genuine privilege increase).

> Operational note: during this session the exemption was applied **live** to unblock testing (an
> explicitly-authorized one-off). The **durable** fix is to add the namespace to the policy's
> exclude list **through GitOps**, not a live `kubectl edit`. See
> [operations.md](operations.md#kyverno-exemption).

The buildcat memory captures this as a reusable platform fact: *Kyverno blocks rootless buildkit;
exempt the daemon namespace (precedent: arc-runners).*

## The control plane is locked down

The friction above is **only** the build daemon. `buildd` itself is an ordinary controller and runs
fully restricted (from the Helm chart):

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  seccompProfile: { type: RuntimeDefault }
  capabilities: { drop: [ALL] }
```

It needs no volumes and no privileges — it only reads/writes CRDs and hands ConfigMap/Secret
**names** to the daemon pods it renders. RBAC is scoped to its own CRDs plus the
StatefulSet/Service/PVC/VolumeSnapshot/Lease verbs it actually uses.

## Where buildcat is actually *more* secure than a shared daemon

The daemon posture is identical to a shared service; the improvement is in **blast radius and
isolation**, which a single shared `buildkitd` cannot offer:

| Risk on a shared daemon | buildcat |
|---|---|
| **Cross-project cache poisoning** — any project's build can write cache that another project reads. | Each `(project, arch)` gets its **own daemon and its own PVC**. There is no shared writable cache to poison across projects. |
| **Untrusted fork PRs** run with the same cache-write access as trusted builds. | `untrusted: true` routes to a `ForkKey` daemon: **ephemeral, seeded read-only from the project snapshot, with no write-back**. A malicious fork cannot poison the project's warm cache. The fork spec comes from the shared `DeriveChild(parent, snapshot, ForkChild, key)` policy — the *same* derivation the M5 fan-out uses (`CloneChild`), so isolation behaviour can't silently diverge between the two paths. |
| **Noisy-neighbour / contention** — one heavy build starves others sharing the daemon. | Dedicated daemon per project; no sharing of CPU/store with unrelated builds. |
| **mTLS endpoint is a single shared trust domain.** | Per-daemon Service; the daemon cert can be scoped, and fork daemons are separate endpoints. |

## Honest tradeoffs

- **Same daemon hardening ceiling.** buildcat does **not** make the buildkit daemon more locked down
  than the shared service — both must relax `no_new_privs`. If your threat model cannot tolerate
  that at all, the answer is a sandboxed runtime (gVisor/Kata/Sysbox) or VM-isolated builders, which
  is orthogonal to buildcat and applies equally to any buildkit deployment.
- **Public exposure is one shared gateway LB.** Daemons stay `ClusterIP`; off-cluster CI reaches all
  of them through a **single** SNI gateway LoadBalancer (not one LB per daemon), so external surface
  is fixed and small regardless of project count. mTLS stays end-to-end — the gateway terminates no
  TLS. Keep even that LB off (in-cluster runners only) when you don't need internet-facing builds.
- **The live exemption is platform state.** The Kyverno exemption must be tracked in GitOps; an
  undocumented live edit is config drift.

See [comparison-buildkit-service.md](comparison-buildkit-service.md) for the full side-by-side.
