#!/usr/bin/env bash
# Runs every fuzz target for a short time. Usage: scripts/fuzz.sh [duration]
set -euo pipefail
time="${1:-10s}"
cd "$(dirname "$0")/.."
# The lists are read into variables first: a failure inside a for loop's
# word list, or before `|| true`, would otherwise fuzz nothing and pass.
pkgs="$(go list ./...)"
for pkg in $pkgs; do
  list="$(go test -list '^Fuzz' "$pkg")"
  for fn in $(grep '^Fuzz' <<<"$list" || true); do
    echo "==> $pkg $fn"
    go test "$pkg" -run '^$' -fuzz "^${fn}\$" -fuzztime "$time"
  done
done
