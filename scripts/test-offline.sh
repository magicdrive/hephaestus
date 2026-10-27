#!/usr/bin/env bash
# test-offline.sh [go test arguments]
#
# Runs the unit and integration tests (default: go test -count=1 ./...)
# without any network: in a new Linux network namespace that has only a
# loopback interface, with GOPROXY=off, GOFLAGS=-mod=mod and
# GOTOOLCHAIN=local, so neither a test nor the go command can reach the
# outside. Modules are downloaded first, outside the namespace: that is the
# only network use, and it is not part of the tests.
#
# Before the tests it checks that the namespace really is offline (no route
# to an outside address, no name resolution) and that loopback works, so the
# job cannot pass because the isolation silently failed.
#
# Linux only. Needs unshare and ip (util-linux, iproute2), and root or sudo;
# the tests themselves run as the calling user.
set -euo pipefail

[[ "$(uname -s)" == Linux ]] || { echo "test-offline.sh: Linux only" >&2; exit 2; }
args=("$@")
((${#args[@]})) || args=(-count=1 ./...)

# With network, outside the namespace: everything the tests build with.
go mod download
go list -deps -test ./... >/dev/null

uid="$(id -u)" gid="$(id -g)"
sudo=()
((uid == 0)) || sudo=(sudo)

# The environment of the tests: explicit, nothing else is inherited.
envs=(
  "HOME=$HOME" "PATH=$PATH" "TMPDIR=${TMPDIR:-/tmp}" "LANG=C.UTF-8"
  "GOCACHE=$(go env GOCACHE)" "GOMODCACHE=$(go env GOMODCACHE)" "GOPATH=$(go env GOPATH)"
  "CGO_ENABLED=${CGO_ENABLED:-0}"
  "GOPROXY=off" "GOFLAGS=-mod=mod" "GOTOOLCHAIN=local"
)

# shellcheck disable=SC2016 # the script runs in the namespace's bash
"${sudo[@]}" unshare --net -- bash -c '
  set -euo pipefail
  ip link set lo up
  if timeout 5 bash -c "exec 3<>/dev/tcp/1.1.1.1/443" 2>/dev/null; then
    echo "test-offline.sh: the namespace reaches 1.1.1.1:443; it is not offline" >&2; exit 2
  fi
  if timeout 5 getent hosts proxy.golang.org >/dev/null 2>&1; then
    echo "test-offline.sh: the namespace resolves proxy.golang.org; it is not offline" >&2; exit 2
  fi
  # Kernels add fallback tunnel devices (down, no address) to every new
  # namespace; what matters is that nothing but loopback has a route.
  if [[ -n "$(ip -4 route show; ip -6 route show | grep -v " dev lo " || true)" ]]; then
    echo "test-offline.sh: the namespace has routes other than loopback" >&2; ip route show >&2; exit 2
  fi
  echo "test-offline.sh: network namespace has loopback only; running the tests offline" >&2
  uid="$1" gid="$2"; shift 2
  if [[ "$uid" == 0 ]]; then exec env -i "$@"; fi
  exec setpriv --reuid="$uid" --regid="$gid" --init-groups env -i "$@"
' test-offline "$uid" "$gid" "${envs[@]}" go test "${args[@]}"
