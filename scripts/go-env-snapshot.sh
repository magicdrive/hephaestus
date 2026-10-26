#!/usr/bin/env bash
# Prints the job's Go environment as a step sees it: PATH, which go, its
# version, GOTOOLCHAIN and GOROOT as go reports them, and every GO* and CGO_*
# variable. CI (action-isolation) compares the output before and after the
# action runs: the action must not change the caller's Go toolchain.
set -euo pipefail
echo "PATH=$PATH"
if go="$(command -v go)"; then
  echo "which go=$go"
  # Both run outside any module, so that a go.mod does not select a
  # toolchain: in this repository, go.mod's toolchain line would make an
  # older Go with GOTOOLCHAIN=auto (a runner's preinstalled Go) download that
  # toolchain, which needs the network and reports the downloaded Go instead
  # of the caller's.
  # Assigned first: a failure inside $(...) in echo's arguments would be
  # ignored and print an empty value.
  version="$(cd / && go version)"
  echo "go version=$version"
  env="$(cd / && go env GOVERSION GOTOOLCHAIN GOROOT | tr -d '\r' | tr '\n' ' ')"
  echo "go env=$env"
else
  echo "which go=<none>"
fi
for v in $(compgen -e | LC_ALL=C sort); do
  case "$v" in GO* | CGO_*) echo "$v=${!v}" ;; esac
done
