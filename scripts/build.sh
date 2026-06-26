#!/usr/bin/env sh
# Core build step behind the buildkit-operator GitHub Action (and usable directly on any CI that can
# run docker buildx + curl + jq). It routes a build to its hot buildkitd and runs `docker buildx`
# against that daemon over mTLS. The cold cache, if buildd has one, is applied automatically from the
# /route response — no S3 config or credentials on this side.
#
# Inputs come from the environment (the Action maps its inputs to these):
#   BUILDKIT_OPERATOR_BUILDD_URL   buildd /route API           (required)
#   BUILDKIT_OPERATOR_CA/CERT/KEY  client mTLS material, PEM   (required)
#   BUILDKIT_OPERATOR_TOKEN        bearer token for /route     (when buildd requires auth)
#   REPO                           project identity            (default: git origin)
#   NAME                           optional monorepo component
#   ARCH                           amd64 | arm64               (default amd64)
#   BUILD_CONTEXT                  build context               (default .)
#   DOCKERFILE / TARGET            optional
#   UNTRUSTED                      true | false                (default false; fork-PR isolation)
#   TAGS                           image tag(s), whitespace-separated (required)
#   PUSH                           true | false                (default false)
set -eu

: "${BUILDKIT_OPERATOR_BUILDD_URL:?set BUILDKIT_OPERATOR_BUILDD_URL}"
REPO="${REPO:-$(git config --get remote.origin.url 2>/dev/null || basename "$PWD")}"
ARCH="${ARCH:-amd64}"
NAME="${NAME:-}"
TARGET="${TARGET:-}"
BUILD_CONTEXT="${BUILD_CONTEXT:-.}"
case "${UNTRUSTED:-false}" in true | 1 | yes) UNTRUSTED=true ;; *) UNTRUSTED=false ;; esac

# api POSTs $2 (JSON) to the buildd path $1, adding the bearer token when one is configured.
api() {
  if [ -n "${BUILDKIT_OPERATOR_TOKEN:-}" ]; then
    curl -fsS -XPOST "$BUILDKIT_OPERATOR_BUILDD_URL$1" -H 'content-type: application/json' \
      -H "authorization: Bearer $BUILDKIT_OPERATOR_TOKEN" -d "$2"
  else
    curl -fsS -XPOST "$BUILDKIT_OPERATOR_BUILDD_URL$1" -H 'content-type: application/json' -d "$2"
  fi
}

# Client mTLS material → a private temp dir (buildx reads the files at create time). Accepts either raw
# PEM (GitHub secrets — multi-line OK) or a base64-encoded blob (GitLab masked variables can't hold
# multi-line PEM, so the convention there is `base64 -w0` + masked_and_hidden) — auto-detected.
certs="$(mktemp -d)"
trap 'rm -rf "$certs"' EXIT
wrcert() { # $1 dest file, $2 value (PEM or base64)
  case "$2" in
    -----BEGIN*) printf '%s' "$2" > "$1" ;;
    *) printf '%s' "$2" | base64 -d > "$1" ;;
  esac
}
wrcert "$certs/ca.pem"   "${BUILDKIT_OPERATOR_CA:?}"
wrcert "$certs/cert.pem" "${BUILDKIT_OPERATOR_CERT:?}"
wrcert "$certs/key.pem"  "${BUILDKIT_OPERATOR_KEY:?}"

# 1. Route: ask buildd for this project's daemon endpoint (+ optional cold-cache reference). target is
# part of the cache identity, so it MUST be sent (else two targets of one repo collide on one daemon).
route_payload="$(jq -nc \
  --arg repo "$REPO" \
  --arg name "$NAME" \
  --arg target "$TARGET" \
  --arg arch "$ARCH" \
  --argjson untrusted "$UNTRUSTED" \
  '{repo:$repo,name:$name,target:$target,arch:$arch,untrusted:$untrusted}')"
resp="$(api /route "$route_payload")"
endpoint="$(printf '%s' "$resp" | jq -r .endpoint)"
key="$(printf '%s' "$resp" | jq -r .key)"
echo "buildkit-operator: routed $REPO${NAME:+/$NAME} ($ARCH${UNTRUSTED:+, untrusted=$UNTRUSTED}) -> $endpoint"

# Per-client gateway override: when this runner reaches the gateway under a DIFFERENT domain/port than
# the one buildd advertises (a multi-domain gateway — e.g. a public domain vs a CI-platform domain
# behind a proxy), rebuild the endpoint from the returned key + this client's host/port. The daemon
# name is buildkitd-<key>; the gateway accepts the SNI as long as the domain is in its --domain list.
if [ -n "${BUILDKIT_OPERATOR_GATEWAY_HOST:-}" ]; then
  gwport="${BUILDKIT_OPERATOR_GATEWAY_PORT:-$(printf '%s' "$endpoint" | sed -E 's#.*:##')}"
  endpoint="tcp://buildkitd-${key}.${BUILDKIT_OPERATOR_GATEWAY_HOST}:${gwport}"
  echo "buildkit-operator: gateway override -> $endpoint"
fi

# Release the inflight build buildd counted on /route so the daemon can scale to zero once idle
# (best-effort; buildd's safety net bounds a missed release). Fires on any exit after routing.
cleanup() {
  rm -rf "$certs"
  if [ -n "${key:-}" ] && [ "$key" != null ]; then
    complete_payload="$(jq -nc --arg key "$key" '{key:$key}')" || return 0
    api /complete "$complete_payload" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Optional: when there is no wildcard DNS for the gateway yet, map this build's gateway hostname to
# the gateway LoadBalancer IP for the duration of the run (testing/bootstrap escape hatch).
if [ -n "${GATEWAY_IP:-}" ]; then
  host="$(printf '%s' "$endpoint" | sed -E 's#^tcp://##; s#:[0-9]+$##')"
  echo "$GATEWAY_IP $host" | sudo tee -a /etc/hosts >/dev/null 2>&1 || echo "$GATEWAY_IP $host" >> /etc/hosts 2>/dev/null || true
  echo "buildkit-operator: mapped $host -> $GATEWAY_IP (no wildcard DNS)"
fi

# 2. Point a buildx remote builder at it over mTLS.
# Egress-proxy tunnel (opt-in: BUILDKIT_OPERATOR_TUNNEL=1 + BUILDKIT_OPERATOR_HTTP_PROXY=host:port) — for
# runners that can only reach the daemon through an HTTP CONNECT proxy (e.g. a CI platform on 443).
# socat tunnels the daemon host:port through the proxy; buildx then connects to the local tunnel but
# still validates TLS against the REAL daemon hostname (servername), so mTLS stays end-to-end.
drv_addr="$endpoint"; srv_opt=""
if [ "${BUILDKIT_OPERATOR_TUNNEL:-}" = "1" ] && [ -n "${BUILDKIT_OPERATOR_HTTP_PROXY:-}" ]; then
  bk="${endpoint#tcp://}"; bkhost="${bk%%:*}"; bkport="${bk##*:}"
  phost="${BUILDKIT_OPERATOR_HTTP_PROXY%%:*}"; pport="${BUILDKIT_OPERATOR_HTTP_PROXY##*:}"
  command -v socat >/dev/null 2>&1 || apk add --no-cache socat >/dev/null 2>&1 || true
  socat TCP-LISTEN:18080,fork,reuseaddr "PROXY:${phost}:${bkhost}:${bkport},proxyport=${pport}" &
  sleep 2
  drv_addr="tcp://127.0.0.1:18080"; srv_opt=",servername=${bkhost}"
  echo "buildkit-operator: tunnel via ${phost}:${pport} -> ${bkhost}:${bkport}"
fi
docker buildx rm buildkit-operator >/dev/null 2>&1 || true
docker buildx create --name buildkit-operator --driver remote \
  --driver-opt "cacert=$certs/ca.pem,cert=$certs/cert.pem,key=$certs/key.pem${srv_opt}" \
  "$drv_addr" --use >/dev/null

# 3. Assemble the buildx args. --metadata-file captures the resulting image digest so the Action can
# expose it as an output (downstream sign/scan/deploy by digest).
meta="$certs/meta.json"
set -- buildx build --builder buildkit-operator --metadata-file "$meta"
[ -n "${DOCKERFILE:-}" ] && set -- "$@" --file "$DOCKERFILE"
[ -n "${TARGET:-}" ] && set -- "$@" --target "$TARGET"
for t in ${TAGS:?set TAGS}; do set -- "$@" --tag "$t"; done
[ "${PUSH:-false}" = "true" ] && set -- "$@" --push

# Supply-chain attestations (need a registry output, i.e. PUSH=true): SLSA provenance + SBOM,
# generated by the daemon and pushed alongside the image.
[ -n "${PROVENANCE:-}" ] && set -- "$@" --provenance="$PROVENANCE"
[ "${SBOM:-false}" = "true" ] && set -- "$@" --sbom=true

# Cold cache: buildd hands us the project's cache reference (no creds — the daemon holds them).
if [ "$(printf '%s' "$resp" | jq -r '.cache.type // empty')" = "s3" ]; then
  s3="type=s3,bucket=$(printf '%s' "$resp" | jq -r .cache.bucket),name=$(printf '%s' "$resp" | jq -r .cache.name)"
  rg="$(printf '%s' "$resp" | jq -r '.cache.region // empty')"; [ -n "$rg" ] && s3="$s3,region=$rg"
  ep="$(printf '%s' "$resp" | jq -r '.cache.endpointUrl // empty')"; [ -n "$ep" ] && s3="$s3,endpoint_url=$ep,use_path_style=true"
  set -- "$@" --cache-from "$s3" --cache-to "$s3,mode=max"
  echo "buildkit-operator: S3 cold cache (project-managed) ON"
fi

# Run the build (not exec'd, so the EXIT trap can release the inflight build afterwards).
status=0
docker "$@" "$BUILD_CONTEXT" || status=$?

# Surface the image digest (present once pushed). Echo it always; export it as a GitHub Action output
# when running under Actions so callers can sign/scan/deploy by digest.
if [ "$status" -eq 0 ] && [ -f "$meta" ]; then
  digest="$(jq -r '."containerimage.digest" // empty' "$meta" 2>/dev/null || true)"
  if [ -n "$digest" ]; then
    echo "buildkit-operator: image digest $digest"
    [ -n "${GITHUB_OUTPUT:-}" ] && echo "digest=$digest" >> "$GITHUB_OUTPUT"
  fi
fi
exit "$status"
