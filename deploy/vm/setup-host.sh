#!/usr/bin/env bash
#
# setup-host.sh — prepare an Incus host for buildkit-operator's single-host backend.
#
# Verifies a ZFS storage pool exists, gives the managed bridge a DNS domain (so instances resolve as
# <name>.<domain>, matching the wildcard daemon cert), and creates the two egress network ACLs the
# backend binds to daemons. Idempotent: re-running is safe.
#
# Usage: sudo ./setup-host.sh <zfs-pool> <bridge> <domain>
#   e.g. sudo ./setup-host.sh tank incusbr0 bko.local
#
# The ACL rules are a conservative STARTING POINT — adapt the strict egress allow-list to your registries.

set -o errexit -o nounset -o pipefail

POOL="${1:?usage: setup-host.sh <zfs-pool> <bridge> <domain>}"
BRIDGE="${2:?missing bridge}"
DOMAIN="${3:?missing domain}"

echo "==> Checking Incus + ZFS pool '${POOL}'"
command -v incus >/dev/null || { echo "incus not found; install Incus >= 6"; exit 1; }
if ! incus storage show "${POOL}" >/dev/null 2>&1; then
  echo "ZFS storage pool '${POOL}' not found. Create one, e.g.:"
  echo "  incus storage create ${POOL} zfs source=<zpool-or-blockdev>"
  exit 1
fi

echo "==> Setting DNS domain '${DOMAIN}' on bridge '${BRIDGE}'"
incus network set "${BRIDGE}" dns.domain "${DOMAIN}"

echo "==> Creating baseline egress ACL 'bko-baseline' (trusted daemons)"
incus network acl create bko-baseline 2>/dev/null || true
# Baseline: allow all egress (trusted builds pull from anywhere). Ingress is the daemon's mTLS port only.
incus network acl rule add bko-baseline egress action=allow 2>/dev/null || true

echo "==> Creating strict egress ACL 'bko-fork-strict' (untrusted forks)"
incus network acl create bko-fork-strict 2>/dev/null || true
# Strict: deny egress to the host + private ranges (no lateral movement / metadata), then allow only DNS
# and outbound HTTPS to public registries. Reject everything else. Order matters (first match wins).
incus network acl rule add bko-fork-strict egress action=reject destination=10.0.0.0/8 2>/dev/null || true
incus network acl rule add bko-fork-strict egress action=reject destination=172.16.0.0/12 2>/dev/null || true
incus network acl rule add bko-fork-strict egress action=reject destination=192.168.0.0/16 2>/dev/null || true
incus network acl rule add bko-fork-strict egress action=reject destination=169.254.169.254/32 2>/dev/null || true
incus network acl rule add bko-fork-strict egress action=allow protocol=udp destination_port=53 2>/dev/null || true
incus network acl rule add bko-fork-strict egress action=allow protocol=tcp destination_port=443 2>/dev/null || true
incus network acl rule add bko-fork-strict egress action=reject 2>/dev/null || true

echo "==> Done. Pool='${POOL}' domain='${DOMAIN}' ACLs: bko-baseline, bko-fork-strict"
echo "    Next: mint certs (GATEWAY_HOST=${DOMAIN} ../cert/create-certs.sh), then build the image."
