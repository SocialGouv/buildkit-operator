#!/usr/bin/env bash
# Every flag the chart renders must exist in the binary it is passed to. Go's flag package exits 2 on
# an unknown flag, so a chart that renders one the binary never defined does not misbehave subtly — it
# CrashLoopBackOffs the container, and for the gateway that means every off-cluster build stops. Unit
# tests cannot see this: the chart and the binary are only joined at deploy time.
#
# Renders the chart with the optional features turned ON (that is where the rarely-exercised flags
# live) and checks each rendered flag against the binary's own flag set.
set -euo pipefail
cd "$(dirname "$0")/.."

# Values chosen to render as many conditional flags as possible, not to be a realistic deployment.
VALUES=(
  --set gateway.host=gw.example.com
  --set api.requireBuildId=true
  --set s3.bucket=b --set s3.credsSecret=s3creds
  --set snapshotClassName=snapclass
  --set defaultStorageClass=sc
  --set mirror.enabled=true --set mirror.host=mirror:5000
  --set sandbox.runtimeClass=kata-clh
  --set daemonScheduling.pinArch=true
  --set 'projectDefaults[0].repo=*'
  --set oidc.providers[0].type=github
  --set oidc.providers[0].issuer=https://token.actions.githubusercontent.com
  --set oidc.providers[0].audience=aud
)
rendered=$(helm template flagcheck deploy/helm/buildkit-operator "${VALUES[@]}")

fail=0
for component in buildd gateway; do
  # The container's args, as rendered: "- --flag=value" / "- --flag".
  flags=$(printf '%s' "$rendered" \
    | awk -v img="buildkit-operator-${component}" '
        $0 ~ "image: .*"img { inctr=1 }
        inctr && /^[[:space:]]*- --/ { print $2 }
        inctr && /^[[:space:]]*(ports|volumeMounts|resources|env):/ { inctr=0 }' \
    | sed 's/=.*//' | sed 's/^--//' | sort -u)
  [ -n "$flags" ] || { echo "check-chart-flags: no args rendered for $component — is the awk scrape still right?"; exit 1; }

  # `--help` is an unknown flag itself, so Go prints the usage and exits 2 — that output is the flag set.
  known=$(go run "./cmd/${component}" --help 2>&1 | awk '/^[[:space:]]+-/ { sub(/^-/, "", $1); print $1 }' | sort -u)
  [ -n "$known" ] || { echo "check-chart-flags: could not read $component's flag set"; exit 1; }
  missing=0
  for f in $flags; do
    if ! printf '%s\n' "$known" | grep -qx "$f"; then
      echo "check-chart-flags: the chart passes --$f to $component, which does not define it (the container would crash on startup)"
      missing=1
      fail=1
    fi
  done
  [ "$missing" = 0 ] && echo "check-chart-flags: $component — $(printf '%s\n' "$flags" | wc -w) rendered flags, all defined"
done
exit "$fail"
