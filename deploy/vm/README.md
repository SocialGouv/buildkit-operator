# Single-host backend (`--backend local`) — Incus + ZFS

Run buildkit-operator on **one VM/host**, no Kubernetes: `buildd --backend local` provisions one vanilla
`buildkitd` per `(project, arch)` as an **Incus instance** backed by a retained **ZFS dataset** (the warm
cache), with an in-process scale-to-zero + snapshot loop. The client, the CI Actions, the router and OIDC
are identical to the Kubernetes path — only the provisioner differs (see [ADR 0007](../../docs/adr/0007-vm-backend-incus-zfs.md)).

This directory is the **e2e kit**: install the host, build the daemon image, run `buildd`, smoke-test.
The scripts are a *starting point* — review them for your host (network, pool name, registries) before
running. Nothing here runs in CI; it needs a real host with Incus + a ZFS pool.

## Prerequisites

- A Linux host with **Incus ≥ 6** (or LXD ≥ 5.21) and a **ZFS** storage pool.
- The `buildd` binary built for the host: `devbox run -- task build` (or `go build ./cmd/buildd`).
- The shared mTLS material minted with a **wildcard over the endpoint domain** (see below).

## 1. Host setup — pool + network ACLs

```sh
# Incus with a ZFS pool named "tank" and a managed bridge "incusbr0" with a DNS domain.
sudo ./setup-host.sh tank incusbr0 bko.local
```

`setup-host.sh` verifies the ZFS pool exists, sets the bridge's DNS domain (so instances resolve as
`<name>.bko.local`), and creates two network ACLs:

- `bko-baseline` — attached to canonical (trusted) daemons.
- `bko-fork-strict` — attached to untrusted **fork** daemons: deny egress to the host/RFC1918, allow only
  DNS + HTTPS to public registries (anti-exfiltration). Tune the allow-list to your registries.

## 2. mTLS certs — wildcard over the endpoint domain

The local backend addresses daemons as `tcp://<daemon>.<endpoint-domain>:1234`, so the daemon cert needs
a `*.<endpoint-domain>` SAN. Reuse the repo's cert script with `GATEWAY_HOST` as the domain:

```sh
GATEWAY_HOST=bko.local ../cert/create-certs.sh
sudo mkdir -p /etc/bko/certs && sudo cp ../cert/.certs/{ca,cert,key}.pem /etc/bko/certs/
# client material for the CI runner / smoke test:
mkdir -p ~/.buildkit-operator/certs && cp ../cert/.certs/client/{ca,cert,key}.pem ~/.buildkit-operator/certs/
```

`/etc/bko/certs` is bind-mounted read-only into every instance at `/certs` by `buildd`
(`--local-certs-path`). Treat it as a secret (see the repo's secrets rule).

## 3. Build the daemon image(s)

```sh
# Container image for trusted daemons + a VM image for untrusted forks.
sudo ./build-buildkitd-image.sh v0.31.1
```

This installs `buildkitd`/`buildctl` into a base instance, adds a systemd unit that runs

```
buildkitd --root /var/lib/buildkit --addr tcp://0.0.0.0:1234 \
          --tlscacert /certs/ca.pem --tlscert /certs/cert.pem --tlskey /certs/key.pem
```

and publishes it as the alias `bko-buildkitd` (container) and `bko-buildkitd-vm` (VM). `buildd` adds
`security.nesting`/`security.privileged` to trusted **container** daemons automatically; untrusted forks
run as VMs (`bko-buildkitd-vm`), where the VM is the isolation boundary.

## 4. Run buildd

```sh
sudo cp buildd /usr/local/bin/buildd
sudo cp buildd.service /etc/systemd/system/buildd.service
sudo install -d /etc/bko && sudo cp buildd.env.example /etc/bko/buildd.env   # then edit
sudo systemctl daemon-reload && sudo systemctl enable --now buildd
```

Key flags (see `buildd.service`): `--backend local --incus-pool tank/bko --incus-image bko-buildkitd
--incus-vm-image bko-buildkitd-vm --local-endpoint-domain bko.local --local-certs-path /etc/bko/certs
--local-snapshot-every 30m --local-keep-snapshots 3`. OIDC/auth flags are unchanged from the k8s path.

## 5. Smoke test

```sh
./smoke-test.sh http://localhost:8080 bko.local ~/.buildkit-operator/certs
```

It routes a tiny build, asserts the instance came up, runs a second build of the same repo to prove the
**warm cache is reused**, checks **scale-to-zero** after the idle window, and routes an `--untrusted`
build to prove the **fork runs as a VM on its own (CoW-seeded) dataset**.

## Quick local try with Docker (no Incus, no ZFS)

To try the control plane on any machine with Docker — no Incus, no ZFS — use the **dev** runtime
`--local-runtime docker`: each daemon is a privileged `buildkitd` container, the per-project cache is a
host directory, and buildkitd is published to a deterministic `127.0.0.1:<port>` (so it works regardless
of Docker bridge networking). It has **no VM isolation** (untrusted forks need the Incus runtime) and
durable snapshots are best-effort (run buildd as root, or use Incus+ZFS, for real snapshots).

```sh
buildd --backend local --local-runtime docker \
  --incus-pool /var/lib/bko-data --incus-image moby/buildkit:buildx-stable-1 \
  --local-mount-path /var/lib/buildkit --local-idle-timeout 20s --api-listen 127.0.0.1:8089

# elsewhere: route, then build against the returned endpoint via the buildx remote driver
ep=$(curl -fsS -XPOST localhost:8089/route -d '{"repo":"demo/app","arch":"amd64"}' | jq -r .endpoint)
docker buildx create --name bko --driver remote "$ep"
docker buildx build --builder bko .
```

This path is validated end-to-end (route → provision → warm cache-mount reuse → scale-to-zero → restart
from the retained cache → cache still warm). It is for local/dev use; production single-host is the Incus
+ ZFS runtime above.

## Cheap cloud test (OVH Public Cloud, billed hourly)

To validate the **real Incus + ZFS** path (kernel snapshots, `zfs clone` CoW, VM forks) without touching
your machine, spin up a small Ubuntu 24.04 instance billed by the hour (OVH Public Cloud exposes the
`vmx` flag, so `/dev/kvm` / nested virt works — both Incus containers *and* VM forks), run the test, and
destroy it (cents). [`quickstart-insecure.sh`](quickstart-insecure.sh) automates it end to end:

```sh
# on your dev box: build the portable binary + copy the harness over
CGO_ENABLED=0 devbox run -- go build -o deploy/vm/buildd ./cmd/buildd
scp deploy/vm/buildd deploy/vm/quickstart-insecure.sh ubuntu@<instance>:/tmp/

# on the instance (Ubuntu 24.04):
sudo /tmp/quickstart-insecure.sh
```

It sets up a loopback ZFS pool + Incus, builds an **insecure** (plaintext, dev-only) buildkitd image,
runs buildd, and checks: cold build → warm cache-mount HIT → ZFS durability snapshots → (if `/dev/kvm`)
an untrusted **VM fork** on its own CoW-cloned dataset. Teardown is printed at the end. This is a dev
harness (no mTLS); production uses the mTLS + ACL path above. (A plain Ubuntu host avoids the
AppArmor/LXC container-start quirks seen on some desktop distros like Mint.)

## Validated on (real Incus + ZFS)

End-to-end validated on **2026-06-29** on a throwaway **OVH Public Cloud** instance:

- Provider / machine: **OVH Public Cloud, flavor `d2-8`** (4 vCPU / 8 GB), region **GRA11 (1-AZ)**,
  hourly billing. `/dev/kvm` present (nested virt) → VM forks work.
- OS: **Ubuntu 26.04**, **Incus 6.0.5**, ZFS on a 20 GB loopback pool (`bkopool`), no Docker.
- Driver: `quickstart-insecure.sh` (the dev/insecure harness above).

Observed (the full feature set, matching the k8s backend):

| Behaviour | Evidence |
|---|---|
| Hot daemon per project | `buildkitd-<key>` RUNNING (CONTAINER) |
| Warm `RUN --mount=type=cache` reuse | build #1 `CACHE_MISS` → build #2 `CACHE_HIT` |
| Durable ZFS snapshot (atomic, kernel) | `bkopool/cache/<key>@bko-<ts>` |
| Untrusted fork under VM isolation | `buildkitd-fork<key>` RUNNING **VIRTUAL-MACHINE** |
| CoW fork seed (`zfs clone`) | `bkopool/cache/fork<key>` USED ≈1 K / REFER ≈5 M (blocks shared with parent) |

The run also surfaced + fixed two real Incus-6.0 bugs: `incus launch --device` must be `incus init` +
`incus config device add` + `incus start`; and egress-ACL binding is best-effort (a missing ACL no longer
fails a build). The local (desktop) path can hit host firewall quirks (Docker's `br_netfilter` + `FORWARD
DROP`, or a host firewall blocking DHCP on the Incus bridge) — a clean server/VM avoids them.

## What's still manual / future

- **Fan-out** has a tested primitive (`Provisioner.Fanout`) but no automatic saturation trigger yet.
- ACL rules in `setup-host.sh` are a conservative starting point — adapt egress to your registry set.
- Incus DNS for `<name>.<domain>` must be reachable from wherever the client runs (same host/bridge, or
  add a resolver entry). Off-host CI typically runs the client on the same VM.
