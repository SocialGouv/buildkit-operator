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
  --set certManager.enabled=true --set certManager.ca.create=true
  --set gateway.externalPort=443
  --set sandbox.buildkitImage=moby/buildkit:v0.31.1
  --set 'projectDefaults[0].repo=*'
  --set oidc.providers[0].type=github
  --set oidc.providers[0].issuer=https://token.actions.githubusercontent.com
  --set oidc.providers[0].audience=aud
)
rendered=$(helm template flagcheck deploy/helm/buildkit-operator "${VALUES[@]}")

fail=0
for component in buildd gateway; do
  # The container's args, as rendered: "- --flag=value" / "- --flag".
  # Args render either bare (- --flag=v) or QUOTED when the value needs it (- "--flag={\"json\":1}").
  # Missing the quoted form is how this check would quietly pass on the flags most likely to be wrong.
  args=$(printf '%s' "$rendered" \
    | awk -v img="buildkit-operator-${component}" '
        $0 ~ "image: .*"img { inctr=1 }
        # Strip the list marker, any surrounding quotes, and a trailing YAML comment (" # ..." is not
        # part of the value the container receives).
        inctr && /^[[:space:]]*- "?--/ { sub(/^[[:space:]]*- "?/, ""); sub(/[[:space:]]+#.*$/, ""); sub(/"$/, ""); print }
        inctr && /^[[:space:]]*(ports|volumeMounts|resources|env):/ { inctr=0 }')
  flags=$(printf '%s\n' "$args" | sed 's/=.*//' | sed 's/^--//' | sort -u)
  [ -n "$flags" ] || { echo "check-chart-flags: no args rendered for $component — is the awk scrape still right?"; exit 1; }

  # `--help` is an unknown flag itself, so Go prints the usage and exits 2 — that output is the flag set.
  help=$(go run "./cmd/${component}" --help 2>&1)
  known=$(printf '%s' "$help" | awk '/^[[:space:]]+-/ { sub(/^-/, "", $1); print $1 }' | sort -u)
  [ -n "$known" ] || { echo "check-chart-flags: could not read $component's flag set"; exit 1; }
  missing=0
  for f in $flags; do
    if ! printf '%s\n' "$known" | grep -qx "$f"; then
      echo "check-chart-flags: the chart passes --$f to $component, which does not define it (the container would crash on startup)"
      missing=1
      fail=1
    fi
  done
  # A flag the binary parses as an int or a bool also has to RECEIVE one: `flag.Int` refuses "3600.5"
  # and the container crashes exactly as if the flag were unknown.
  while IFS= read -r arg; do
    case "$arg" in
      *=*) name=${arg%%=*}; name=${name#--}; value=${arg#*=} ;;
      *) continue ;;
    esac
    kind=$(printf '%s' "$help" | awk -v f="-$name" '$1 == f { print $2 }')
    case "$kind" in
      int) case "$value" in ''|*[!0-9-]*) echo "check-chart-flags: $component --$name is an int flag but the chart renders \"$value\""; fail=1 ;; esac ;;
    esac
  done <<EOF_ARGS
$args
EOF_ARGS
  [ "$missing" = 0 ] && echo "check-chart-flags: $component — $(printf '%s\n' "$flags" | wc -w) rendered flags, all defined"
done
exit "$fail"
