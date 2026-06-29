#!/usr/bin/env bash
#
# quickstart-insecure.sh — fast, cheap dev e2e of `buildd --backend local` (Incus + ZFS) on a clean
# Ubuntu host, e.g. an OVH Public Cloud instance billed hourly (spin up → test → destroy = cents).
#
# It sets up a loopback ZFS pool + Incus, builds an INSECURE buildkitd image (plaintext tcp — DEV ONLY,
# no mTLS, to skip the cert/DNS dance), runs buildd, and smoke-tests: route → cold build → warm
# cache-mount HIT → ZFS durability snapshots. Untrusted (VM fork) is exercised when /dev/kvm is present.
#
# Prereqs: Ubuntu 24.04, run as root. The buildd binary next to this script — build it on your dev box
# (portable, static) and scp it over:
#     CGO_ENABLED=0 devbox run -- go build -o deploy/vm/buildd ./cmd/buildd
#     scp deploy/vm/buildd deploy/vm/quickstart-insecure.sh ubuntu@<instance>:/tmp/
#
# This is a DEV harness (insecure, loopback pool). For the production single-host setup use the Incus+ZFS
# path in README.md (mTLS, wildcard cert, network ACLs). Untested in CI — read it before running.

set -o errexit -o nounset -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILDD="${BUILDD:-$HERE/buildd}"
BK_VER="${BK_VER:-v0.31.1}"
ARCH="${ARCH:-amd64}"
ZPOOL="${ZPOOL:-bkopool}"
POOL_IMG="${POOL_IMG:-/var/lib/bko-zpool.img}"
POOL_SIZE="${POOL_SIZE:-20G}"
CACHE_DS="$ZPOOL/cache"
API="${API:-127.0.0.1:8089}"
TARBALL_URL="https://github.com/moby/buildkit/releases/download/$BK_VER/buildkit-$BK_VER.linux-$ARCH.tar.gz"

[ "$(id -u)" = 0 ] || { echo "run as root (sudo)"; exit 1; }
[ -x "$BUILDD" ] || { echo "buildd binary not found at $BUILDD — build + scp it (see header)"; exit 1; }

echo "== packages (incus, zfs, buildctl) =="
if ! command -v incus >/dev/null || ! command -v zfs >/dev/null; then
  apt-get update -qq && apt-get install -y -qq incus zfsutils-linux curl
fi
command -v buildctl >/dev/null || curl -fsSL "$TARBALL_URL" | tar -C /usr/local -xz

echo "== ZFS pool on a loopback file ($POOL_IMG) =="
zpool list "$ZPOOL" >/dev/null 2>&1 || { truncate -s "$POOL_SIZE" "$POOL_IMG"; zpool create "$ZPOOL" "$POOL_IMG"; }
zfs list "$CACHE_DS" >/dev/null 2>&1 || zfs create "$CACHE_DS"

echo "== Incus init =="
incus storage list >/dev/null 2>&1 || incus admin init --minimal

build_image() { # alias, extra launch flags (e.g. --vm)
  local alias="$1"; shift
  incus image alias list 2>/dev/null | grep -q " $alias " && return 0
  local tmp="bko-base-$alias"
  incus delete -f "$tmp" 2>/dev/null || true
  echo "   building image $alias $*"
  incus launch images:ubuntu/24.04 "$tmp" -c security.nesting=true "$@"
  for _ in $(seq 1 60); do incus exec "$tmp" -- test -e /run/systemd/system 2>/dev/null && break; sleep 1; done
  incus exec "$tmp" -- bash -c "
    set -e
    curl -fsSL '$TARBALL_URL' | tar -C /usr/local -xz
    mkdir -p /var/lib/buildkit
    cat >/etc/systemd/system/buildkitd.service <<'UNIT'
[Unit]
Description=buildkitd
After=network-online.target
[Service]
ExecStart=/usr/local/bin/buildkitd --root /var/lib/buildkit --addr tcp://0.0.0.0:1234
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
    systemctl enable buildkitd"
  incus stop "$tmp"
  incus publish "$tmp" --alias "$alias"
  incus delete "$tmp"
}

echo "== build insecure buildkitd image(s) =="
build_image bko-buildkitd
VM_FLAG=""
if [ -e /dev/kvm ]; then build_image bko-buildkitd-vm --vm; VM_FLAG="--incus-vm-image bko-buildkitd-vm"; fi

echo "== run buildd (root; insecure; IP endpoint) =="
pkill -f "$BUILDD" 2>/dev/null || true
# shellcheck disable=SC2086
"$BUILDD" --backend local --local-runtime incus \
  --incus-pool "$CACHE_DS" --incus-image bko-buildkitd $VM_FLAG \
  --local-mount-path /var/lib/buildkit --local-idle-timeout 90s \
  --local-snapshot-every 20s --local-keep-snapshots 2 \
  --api-listen "$API" --port 1234 >/var/log/bko-buildd.log 2>&1 &
sleep 3

route() { curl -fsS --max-time 120 -XPOST "http://$API/route" -H 'content-type: application/json' -d "$1"; }
mkdir -p /tmp/bko-ctx
cat >/tmp/bko-ctx/Dockerfile <<'DF'
FROM busybox
ARG X
RUN --mount=type=cache,target=/c sh -c '[ -f /c/x ] && echo ">>> CACHE_HIT" || { echo ">>> CACHE_MISS"; touch /c/x; }; echo "x=$X"'
DF
dobuild() { buildctl --addr "$1" build --frontend dockerfile.v0 --opt "build-arg:X=$2" \
  --local context=/tmp/bko-ctx --local dockerfile=/tmp/bko-ctx 2>&1 | grep -E 'CACHE_'; }

echo "== 1) cold route + build (expect CACHE_MISS) =="
EP=$(route '{"repo":"demo/app","arch":"amd64"}' | sed -E 's/.*"endpoint":"([^"]+)".*/\1/'); echo "   endpoint=$EP"
dobuild "$EP" 1
echo "== 2) second build (busted layer, warm cache MOUNT => CACHE_HIT) =="
dobuild "$EP" 2
echo "== 3) ZFS durability snapshots (after cadence) =="
sleep 25; zfs list -t snapshot 2>/dev/null | grep "$CACHE_DS" || echo "   (none yet — check /var/log/bko-buildd.log)"
if [ -n "$VM_FLAG" ]; then
  echo "== 4) untrusted build => VM fork on its own CoW-cloned dataset =="
  FEP=$(route '{"repo":"demo/app","arch":"amd64","untrusted":true}' | sed -E 's/.*"endpoint":"([^"]+)".*/\1/'); echo "   fork endpoint=$FEP"
  incus list --format csv -c nt 2>/dev/null | grep -i buildkitd || true
fi

echo
echo "== done =="
echo "  logs:     /var/log/bko-buildd.log"
echo "  stop:     pkill -f buildd ; incus list"
echo "  teardown: incus delete -f \$(incus list -c n -f csv | grep buildkitd) ; zpool destroy $ZPOOL ; rm -f $POOL_IMG"
