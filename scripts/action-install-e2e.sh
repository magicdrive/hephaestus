#!/usr/bin/env bash
# End-to-end test of the action's release path (action/install-hephaestus.sh)
# against a published release: the action directory is laid out as the
# runner does (_actions/magicdrive/hephaestus/<ref>), with action/VERSION
# naming the release, and the script downloads the archive for this runner,
# checks the release checksum and the provenance attestation and runs the
# binary. References that do not belong to the release must fail.
#
#   scripts/action-install-e2e.sh VERSION COMMIT WRONG_COMMIT
#
# VERSION is a published release tag, COMMIT the commit it points to and
# WRONG_COMMIT any other commit of the repository. GH_TOKEN must be set
# (read-only) for gh attestation verify.
set -euo pipefail
version=$1 commit=$2 wrong=$3
repo="$(cd "$(dirname "$0")/.." && pwd)"
tmp="${RUNNER_TEMP:-$(mktemp -d)}/action-install-e2e"
rm -rf "$tmp"
mkdir -p "$tmp"

# install REF [ACTION_REF]: lays the action out under _actions/.../REF and
# runs the install script; prints the installed binary's path.
install() {
  local action="$tmp/w/_actions/magicdrive/hephaestus/$1" out="$tmp/out-$1"
  mkdir -p "$action/action" "$tmp/temp-$1"
  cp "$repo/action/install-hephaestus.sh" "$action/action/"
  printf '%s\n' "$version" >"$action/action/VERSION"
  : >"$out"
  ACTION_PATH="$action" ACTION_REF="${2:-}" RUNNER_TEMP="$tmp/temp-$1" GITHUB_OUTPUT="$out" \
    GITHUB_WORKSPACE="$repo" bash "$action/action/install-hephaestus.sh" >&2 || return 1
  sed -n 's/^bin=//p' "$out"
}

failures=0
check() {
  local name=$1
  shift
  if "$@"; then echo "ok   $name"; else
    echo "FAIL $name"
    failures=$((failures + 1))
  fi
}

bin="$(install "$commit")" || bin=""
check "pinned by commit (path): installs" test -n "$bin"
if [[ -n "$bin" ]]; then
  check "pinned by commit (path): reports $version" test "$("$bin" version | tr -d '\r')" = "hephaestus $version"
fi
bin="$(install "$commit" "$commit")" || bin=""
check "pinned by commit (github.action_ref): installs" test -n "$bin"
bin="$(install "$version")" || bin=""
check "referenced by its tag: installs" test -n "$bin"

if install "$wrong" >/dev/null 2>"$tmp/err"; then
  check "another commit is refused" false
else
  check "another commit is refused" grep -q 'attestation' "$tmp/err"
fi
if install main >/dev/null 2>"$tmp/err"; then
  check "a branch is refused" false
else
  check "a branch is refused" grep -q 'referenced as @main' "$tmp/err"
fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "action release path verified for $version at $commit"
