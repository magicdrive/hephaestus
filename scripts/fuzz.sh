#!/usr/bin/env bash
# Runs every fuzz target for a short time. Usage: scripts/fuzz.sh [duration]
set -euo pipefail
time="${1:-10s}"
cd "$(dirname "$0")/.."
for pkg in $(go list ./...); do
  for fn in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
    echo "==> $pkg $fn"
    go test "$pkg" -run '^$' -fuzz "^${fn}\$" -fuzztime "$time"
  done
done
