#!/usr/bin/env sh
# Core build step behind the buildkit-operator GitHub Action (and usable directly on any CI that can
# run docker buildx + curl + jq). It routes a build to its hot buildkitd and runs `docker buildx`
# against that daemon over mTLS. The cold cache, if buildd has one, is applied automatically from the
# /route response — no S3 config or credentials on this side.
#
# Inputs come from the environment (the Action maps its inputs to these):
#   BUILDKIT_OPERATOR_BUILDD_URL   buildd /route API           (required)
#   BUILDKIT_OPERATOR_CA/CERT/KEY  client mTLS material, PEM   (required)
#   REPO                           project identity            (default: git origin)
#   NAME                           optional monorepo component
#   ARCH                           amd64 | arm64               (default amd64)
#   BUILD_CONTEXT                  build context               (default .)
#   DOCKERFILE / TARGET            optional
#   TAGS                           image tag(s), whitespace-separated (required)
#   PUSH                           true | false                (default false)
set -eu

: "${BUILDKIT_OPERATOR_BUILDD_URL:?set BUILDKIT_OPERATOR_BUILDD_URL}"
REPO="${REPO:-$(git config --get remote.origin.url 2>/dev/null || basename "$PWD")}"
ARCH="${ARCH:-amd64}"
NAME="${NAME:-}"
BUILD_CONTEXT="${BUILD_CONTEXT:-.}"

# Client mTLS material → a private temp dir (buildx reads the files at create time).
certs="$(mktemp -d)"
trap 'rm -rf "$certs"' EXIT
printf '%s' "${BUILDKIT_OPERATOR_CA:?}"   > "$certs/ca.pem"
printf '%s' "${BUILDKIT_OPERATOR_CERT:?}" > "$certs/cert.pem"
printf '%s' "${BUILDKIT_OPERATOR_KEY:?}"  > "$certs/key.pem"

# 1. Route: ask buildd for this project's daemon endpoint (+ optional cold-cache reference).
resp="$(curl -fsS -XPOST "$BUILDKIT_OPERATOR_BUILDD_URL/route" \
  -H 'content-type: application/json' \
  -d "{\"repo\":\"$REPO\",\"name\":\"$NAME\",\"arch\":\"$ARCH\"}")"
endpoint="$(printf '%s' "$resp" | jq -r .endpoint)"
echo "buildkit-operator: routed $REPO${NAME:+/$NAME} ($ARCH) -> $endpoint"

# 2. Point a buildx remote builder at it over mTLS.
docker buildx rm buildkit-operator >/dev/null 2>&1 || true
docker buildx create --name buildkit-operator --driver remote \
  --driver-opt "cacert=$certs/ca.pem,cert=$certs/cert.pem,key=$certs/key.pem" \
  "$endpoint" --use >/dev/null

# 3. Assemble the buildx args.
set -- buildx build --builder buildkit-operator
[ -n "${DOCKERFILE:-}" ] && set -- "$@" --file "$DOCKERFILE"
[ -n "${TARGET:-}" ] && set -- "$@" --target "$TARGET"
for t in ${TAGS:?set TAGS}; do set -- "$@" --tag "$t"; done
[ "${PUSH:-false}" = "true" ] && set -- "$@" --push

# Cold cache: buildd hands us the project's cache reference (no creds — the daemon holds them).
if [ "$(printf '%s' "$resp" | jq -r '.cache.type // empty')" = "s3" ]; then
  s3="type=s3,bucket=$(printf '%s' "$resp" | jq -r .cache.bucket),name=$(printf '%s' "$resp" | jq -r .cache.name)"
  rg="$(printf '%s' "$resp" | jq -r '.cache.region // empty')"; [ -n "$rg" ] && s3="$s3,region=$rg"
  ep="$(printf '%s' "$resp" | jq -r '.cache.endpointUrl // empty')"; [ -n "$ep" ] && s3="$s3,endpoint_url=$ep,use_path_style=true"
  set -- "$@" --cache-from "$s3" --cache-to "$s3,mode=max"
  echo "buildkit-operator: S3 cold cache (project-managed) ON"
fi

exec docker "$@" "$BUILD_CONTEXT"
