# Single-host backend (Incus + ZFS)

buildkit-operator is a **control plane over stock `buildkitd`** — routing + lifecycle. That control
plane is **substrate-agnostic**: the router (cache identity), the `build` CLI, the CI Actions and the
OIDC identity binding are all backend-neutral. Only one layer knows *how* a daemon is provisioned — the
**`Provisioner`** ([`internal/provisioner`](https://github.com/socialgouv/buildkit-operator/tree/main/internal/provisioner)) —
so the same operator runs on **Kubernetes** *or* on a **single host**, selected with `--backend`.

See [ADR 0007](adr/0007-vm-backend-incus-zfs.md) for the decision and trade-offs.

## When to use it

- **Kubernetes** (`--backend k8s`, default) — the production target on OVH MKS: horizontal scale across a
  nodepool, HA, declarative reconcile, Cinder PVCs, VolumeSnapshots, Kata for untrusted forks.
- **Single host** (`--backend local`) — a self-hosted / on-prem / edge box, or a dev machine, where a
  cluster is overkill or unavailable. One hot `buildkitd` per project as an **Incus instance** backed by a
  retained **ZFS dataset** (the warm cache), with an in-process scale-to-zero + snapshot loop. No CRD, no
  etcd — a single process. Cold start off local ZFS avoids the Cinder network-attach latency that
  dominates the Kubernetes cold start.

The trade-off is inherent: a single host caps total concurrency at its own CPU/RAM/disk and has no HA.
That is acceptable — and the point — for the single-VM audience.

## How it maps

The Kubernetes feature set maps almost 1:1 onto Incus + ZFS, often more simply:

| Kubernetes | Incus + ZFS |
|---|---|
| StatefulSet-of-1 (one buildkitd) | one Incus **instance** (container = trusted; **VM** = untrusted fork) |
| Service DNS / SNI gateway endpoint | `<daemon>.<domain>` (Incus DNS) or the instance IP |
| Retained Cinder PVC (warm cache) | retained **ZFS dataset** |
| `VolumeSnapshot` (durability) | `zfs snapshot` (atomic, kernel) |
| CoW fork seed (snapshot + restore) | `zfs clone` (instant, shares blocks) |
| Kata microVM (untrusted isolation) | Incus **VM** instance |
| Scale-to-zero (replicas 0/1) | `incus stop` / `incus start`; dataset kept |
| NetworkPolicy (egress) | Incus network ACL (strict on untrusted forks) |

## Runtimes

`--local-runtime` selects how instances are realised:

- **`incus`** (production single-host) — Incus + ZFS. Untrusted forks run as **VMs** (the hypervisor is
  the isolation boundary); durable ZFS snapshots; CoW fork seeding.
- **`docker`** (dev/local) — privileged `buildkitd` containers, host-directory caches, buildkitd published
  to a deterministic `127.0.0.1:<port>`. Needs neither Incus nor ZFS. **No VM isolation** (untrusted forks
  require the Incus runtime) and snapshots are best-effort. For trying the control plane on any machine.

## Install & run

The full kit — host setup, the buildkitd image, certs, the `buildd` systemd unit and a smoke test — lives
in **[`deploy/vm/`](https://github.com/socialgouv/buildkit-operator/tree/main/deploy/vm)**
(`deploy/vm/README.md`). A one-shot dev harness, `quickstart-insecure.sh`, stands the whole thing up on a
clean Ubuntu host (or a throwaway cloud VM) and runs an end-to-end check.

```sh
buildd --backend local --local-runtime incus \
  --incus-pool tank/bko --incus-image bko-buildkitd --incus-vm-image bko-buildkitd-vm \
  --local-endpoint-domain bko.local --local-certs-path /etc/bko/certs \
  --local-mount-path /var/lib/buildkit --local-idle-timeout 15m \
  --local-snapshot-every 30m --local-keep-snapshots 3
```

The `build` CLI, the GitHub/GitLab/Forgejo Actions and OIDC are **identical** to the Kubernetes path — a
client only ever sees `POST /route` then `docker buildx --driver remote` against an mTLS endpoint.

## Validated

End-to-end on real Incus + ZFS (OVH Public Cloud `d2-8`, Ubuntu 26.04, Incus 6.0.5) and on a local
desktop: hot daemon, warm `RUN --mount=type=cache` reuse, durable kernel ZFS snapshots with retention,
scale-to-zero + restart from the retained cache, an untrusted fork running as a **VM** on a CoW-cloned
dataset, and the mTLS path (wildcard cert + endpoint domain). Indicative timings: warm route + trivial
build ≈ 2 s; cold (provision + `apt install`) ≈ 20 s; warm rebuild reusing the apt cache mount ≈ 12 s.

## What's deferred

A `Fanout` clone primitive exists and is unit-tested, but no automatic saturation **trigger** invokes it
yet. The Docker runtime is dev-only (no VM isolation, best-effort snapshots).
