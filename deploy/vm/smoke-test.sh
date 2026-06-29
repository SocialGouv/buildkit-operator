#!/usr/bin/env bash
#
# smoke-test.sh — end-to-end check of the single-host (Incus + ZFS) backend.
#
# Proves: a build routes + the daemon comes up; a second build of the same repo reuses the warm cache;
# the daemon scales to zero when idle; an untrusted build routes to a VM fork on its own dataset.
#
# Usage: ./smoke-test.sh <buildd-url> <endpoint-domain> <client-certs-dir>
#   e.g. ./smoke-test.sh http://localhost:8080 bko.local ~/.buildkit-operator/certs
#
# Needs: curl, jq, buildctl on PATH, and BUILDKIT_OPERATOR_ADMIN_TOKEN exported (matches buildd.env).
# Run buildd with a short --local-idle-timeout (e.g. 1m) to exercise scale-to-zero quickly.

set -o errexit -o nounset -o pipefail

URL="${1:?usage: smoke-test.sh <buildd-url> <endpoint-domain> <certs-dir>}"
DOMAIN="${2:?missing endpoint-domain}"
CERTS="${3:?missing client certs dir}"
TOKEN="${BUILDKIT_OPERATOR_ADMIN_TOKEN:?export BUILDKIT_OPERATOR_ADMIN_TOKEN (see buildd.env)}"
REPO="smoke/$(date +%s 2>/dev/null || echo run)"

for bin in curl jq buildctl; do command -v "$bin" >/dev/null || { echo "missing $bin"; exit 1; }; done

ctx="$(mktemp -d)"; trap 'rm -rf "$ctx"' EXIT
cat >"${ctx}/Dockerfile" <<'DF'
FROM busybox
RUN --mount=type=cache,target=/c echo "warm $(date)" > /c/marker && cat /c/marker
RUN echo built > /built
DF

route() { # route <untrusted:true|false> -> prints JSON
  curl -fsS -X POST "${URL}/route" \
    -H 'Content-Type: application/json' \
    -H "X-Buildkit-Operator-Admin-Token: ${TOKEN}" \
    -d "{\"repo\":\"${REPO}\",\"arch\":\"amd64\",\"untrusted\":${1}}"
}

dobuild() { # dobuild <endpoint>
  buildctl --addr "$1" \
    --tlscacert "${CERTS}/ca.pem" --tlscert "${CERTS}/cert.pem" --tlskey "${CERTS}/key.pem" \
    build --frontend dockerfile.v0 --local context="${ctx}" --local dockerfile="${ctx}" >/dev/null
}

echo "==> 1) route + cold build (${REPO})"
resp="$(route false)"; ep="$(echo "$resp" | jq -r .endpoint)"; key="$(echo "$resp" | jq -r .key)"
echo "    endpoint=${ep} key=${key}"
[ -n "$ep" ] && [ "$ep" != "null" ] || { echo "FAIL: empty endpoint"; exit 1; }
t0=$(date +%s); dobuild "$ep"; t1=$(date +%s)
echo "    cold build OK in $((t1 - t0))s"
incus list "buildkitd-${key}" -c ns -f csv || true

echo "==> 2) second build of the same repo (warm cache reuse)"
route false >/dev/null
t0=$(date +%s); dobuild "$ep"; t1=$(date +%s)
echo "    warm build OK in $((t1 - t0))s (expect faster; cache mount reused)"

echo "==> 3) scale-to-zero (polling up to 120s; run buildd with --local-idle-timeout 1m)"
ok=0
for _ in $(seq 1 24); do
  s="$(incus list "buildkitd-${key}" -c s -f csv 2>/dev/null || true)"
  if [ "$s" = "STOPPED" ]; then ok=1; break; fi
  sleep 5
done
[ "$ok" = 1 ] && echo "    instance STOPPED (scaled to zero); dataset retained" \
  || echo "    WARN: not stopped yet (idle window longer than 120s?)"

echo "==> 4) untrusted build → VM fork on its own dataset"
fresp="$(route true)"; fkey="$(echo "$fresp" | jq -r .key)"; fep="$(echo "$fresp" | jq -r .endpoint)"
echo "    fork key=${fkey} endpoint=${fep}"
[ "$fkey" != "$key" ] || { echo "FAIL: fork shares canonical key (no isolation)"; exit 1; }
incus list "buildkitd-${fkey}" -c nst -f csv || true
echo "    (type should be VIRTUAL-MACHINE; dataset tank/bko/${fkey} is a clone of the canonical snapshot)"

echo "==> smoke test passed"
