#!/usr/bin/env bash
# Prints the job's Go environment as a step sees it: PATH, which go, its
# version, GOTOOLCHAIN and GOROOT as go reports them, and every GO* and CGO_*
# variable. CI (action-isolation) compares the output before and after the
# action runs: the action must not change the caller's Go toolchain.
set -euo pipefail
echo "PATH=$PATH"
if go="$(command -v go)"; then
  echo "which go=$go"
  # Assigned first: a failure inside $(...) in echo's arguments would be
  # ignored and print an empty value (e.g. a failed toolchain download).
  version="$(go version)"
  echo "go version=$version"
  # Outside any module, so that a go.mod does not select a toolchain.
  env="$(cd / && go env GOVERSION GOTOOLCHAIN GOROOT | tr -d '\r' | tr '\n' ' ')"
  echo "go env=$env"
else
  echo "which go=<none>"
fi
for v in $(compgen -e | LC_ALL=C sort); do
  case "$v" in GO* | CGO_*) echo "$v=${!v}" ;; esac
done
