#!/usr/bin/env bash
#
# build-buildkitd-image.sh — build the Incus image(s) that run vanilla buildkitd for the local backend.
#
# Produces two published image aliases:
#   bko-buildkitd      — system container, for trusted canonical daemons
#   bko-buildkitd-vm   — VM, for untrusted fork daemons (the VM is the isolation boundary)
#
# Each image bundles buildkitd + buildctl (vanilla, from the upstream release) and a systemd unit that
# serves mTLS on :1234, with --root /var/lib/buildkit (where buildd mounts the per-project ZFS dataset)
# and certs read from /certs (where buildd bind-mounts the host cert dir). NO custom plugins.
#
# Usage: sudo ./build-buildkitd-image.sh <buildkit-version>   e.g. sudo ./build-buildkitd-image.sh v0.31.1

set -o errexit -o nounset -o pipefail

VER="${1:?usage: build-buildkitd-image.sh <buildkit-version, e.g. v0.31.1>}"
BASE_IMG="${BASE_IMG:-images:debian/12}"
ARCH="$(uname -m)"; case "${ARCH}" in x86_64) BK_ARCH=amd64;; aarch64) BK_ARCH=arm64;; *) BK_ARCH="${ARCH}";; esac
TARBALL="buildkit-${VER}.linux-${BK_ARCH}.tar.gz"
URL="https://github.com/moby/buildkit/releases/download/${VER}/${TARBALL}"

# provision NAME: install buildkit + the systemd unit inside a running instance, then stop it.
provision() {
  local name="$1"
  echo "==> [${name}] installing buildkit ${VER}"
  incus exec "${name}" -- sh -eu -c "
    apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null
    curl -fsSL '${URL}' -o /tmp/bk.tgz
    tar -C /usr/local -xzf /tmp/bk.tgz   # unpacks bin/buildkitd, bin/buildctl
    install -d /var/lib/buildkit /certs
    cat >/etc/systemd/system/buildkitd.service <<'UNIT'
[Unit]
Description=buildkitd (buildkit-operator local backend)
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/buildkitd --root /var/lib/buildkit --addr tcp://0.0.0.0:1234 --tlscacert /certs/ca.pem --tlscert /certs/cert.pem --tlskey /certs/key.pem
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
    systemctl enable buildkitd.service >/dev/null 2>&1 || true
  "
}

build_one() {
  local alias="$1"; shift   # remaining args: extra `incus launch` flags (e.g. --vm)
  local tmp="bko-build-$$-${alias}"
  echo "==> Building image '${alias}' from ${BASE_IMG} $*"
  incus launch "${BASE_IMG}" "${tmp}" "$@" -c security.nesting=true >/dev/null
  # Wait for the instance to boot + get networking before apt.
  for _ in $(seq 1 30); do incus exec "${tmp}" -- test -e /run/systemd/system && break; sleep 1; done
  provision "${tmp}"
  incus stop "${tmp}"
  incus image delete "${alias}" 2>/dev/null || true
  incus publish "${tmp}" --alias "${alias}"
  incus delete "${tmp}"
  echo "==> Published image alias '${alias}'"
}

build_one bko-buildkitd
build_one bko-buildkitd-vm --vm

echo "==> Done: images bko-buildkitd (container) + bko-buildkitd-vm (VM)"
