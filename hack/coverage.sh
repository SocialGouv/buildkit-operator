#!/usr/bin/env bash
# coverage.sh — statement-weighted coverage gate over a Go cover profile.
#
# It measures coverage over the *unit-testable* code only, excluding:
#   - generated files (zz_generated*.go)
#   - process entrypoints / runtime bootstrap that belong to e2e, not unit tests
#     (main, newRootCmd, Start, SetupWithManager) — see DENY below.
#
# Exclusion is statement-weighted and accurate: a function's profile blocks are dropped by computing
# its line range [funcStart, nextFuncStartInSameFile) from `go tool cover -func`. The remaining blocks
# are summed (covered statements / total statements). Fails if the result is below THRESHOLD.
#
# Usage: coverage.sh <profile> <threshold-percent>
set -euo pipefail

PROFILE="${1:?usage: coverage.sh <profile> <threshold>}"
THRESHOLD="${2:?usage: coverage.sh <profile> <threshold>}"

# Function names treated as non-unit-testable bootstrap / runtime glue (space-separated):
#   main, newRootCmd, run, Start, SetupWithManager — process entrypoints + manager/server bootstrap.
#   warnIfDaemonCertMissingGatewaySAN — boot-time Secret read; its pure core (certCoversGateway) is tested.
DENY="main newRootCmd run Start SetupWithManager warnIfDaemonCertMissingGatewaySAN"

FUNCS="$(go tool cover -func="$PROFILE")"

pct="$(
  awk -v deny="$DENY" '
    BEGIN { split(deny, d, " "); for (i in d) denied[d[i]] = 1 }

    # Pass 1: func report ("path/file.go:line:\tFuncName\t12.3%"), in source order. Record each
    # functions start line per file so we can derive its [start, nextStart) line range.
    NR == FNR {
      if ($0 ~ /^total:/) next
      split($1, a, ":")
      file = a[1]; line = a[2] + 0; name = $2
      idx = ++count[file]
      fstart[file, idx] = line
      fname[file, idx] = name
      next
    }

    # Pass 2: the cover profile. Skip the "mode:" header; each block is "file:sL.sC,eL.eC numStmt count".
    $1 == "mode:" { next }
    {
      colon = match($1, /:[0-9]+\.[0-9]+,/)
      if (colon == 0) next
      file = substr($1, 1, colon - 1)
      sl = substr($1, colon + 1) + 0   # start line of the block
      n = $2 + 0; cnt = $3 + 0

      if (file ~ /zz_generated/) next

      drop = 0; m = count[file]
      for (i = 1; i <= m; i++) {
        if (denied[fname[file, i]]) {
          lo = fstart[file, i]
          hi = (i < m) ? fstart[file, i + 1] : 2147483647
          if (sl >= lo && sl < hi) { drop = 1; break }
        }
      }
      if (drop) next

      total += n
      if (cnt > 0) covered += n
    }

    END {
      if (total == 0) { print "0.0"; exit }
      printf "%.1f", (covered / total) * 100
    }
  ' <(printf '%s\n' "$FUNCS") "$PROFILE"
)"

echo "unit-testable coverage (excl. generated + bootstrap): ${pct}% (threshold ${THRESHOLD}%)"
awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { exit !(p + 0 >= t + 0) }' || {
  echo "FAIL: coverage ${pct}% is below the ${THRESHOLD}% threshold" >&2
  exit 1
}
