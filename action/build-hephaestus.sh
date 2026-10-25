#!/usr/bin/env bash
# Builds the hephaestus binary for the composite action (action.yml) from the
# action's own source, with a Go toolchain private to this script.
#
# The caller's job is not changed: nothing is written to $GITHUB_PATH or
# $GITHUB_ENV, and no Go is put on the job's PATH. Build commands of the
# project (go build, cargo, ...) therefore run with the toolchain the caller
# set up, not with the Go that built Hephaestus. (v1.1.0 used
# actions/setup-go here, which exported its Go and GOTOOLCHAIN=local to the
# rest of the job.)
#
# The toolchain is the one go.mod names in its toolchain line:
#
#   - On GitHub-hosted platforms (Linux, macOS, Windows; x64 and ARM64) the
#     official archive is downloaded from dl.google.com and checked against
#     the SHA-256 pinned in action/go-toolchain.sha256, part of the commit
#     the caller pinned. It is unpacked into a new directory under
#     $RUNNER_TEMP and called by absolute path.
#   - Elsewhere, a go (1.21 or later) already on PATH is used with
#     GOTOOLCHAIN set to that version for this script only; Go downloads the
#     toolchain through the module proxy and verifies it with the checksum
#     database.
#
# The caller's Go settings (earlier steps, workflow env, $GITHUB_ENV) must not
# change how Hephaestus is built either: every GO* and CGO_* variable is
# cleared, then the module proxy and checksum database are pinned, go.work,
# the go env file and private/insecure module patterns are disabled, and new
# GOCACHE and GOMODCACHE directories are used, so no restored cache is read.
#
# Writes bin=<path of the binary> to $GITHUB_OUTPUT.
set -euo pipefail

fail() {
  echo "::error::hephaestus action: $*" >&2
  exit 1
}

: "${ACTION_PATH:?ACTION_PATH is not set}" "${RUNNER_TEMP:?RUNNER_TEMP is not set}" "${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set}"

tmp="$RUNNER_TEMP" src="$ACTION_PATH" bin_ext=""
if [[ "${RUNNER_OS:-}" == "Windows" ]]; then
  # Git Bash tools and the native go.exe both accept D:/a/_temp;
  # RUNNER_TEMP arrives as D:\a\_temp.
  tmp="$(cygpath -m "$RUNNER_TEMP")" src="$(cygpath -m "$ACTION_PATH")" bin_ext=".exe"
fi

version="$(sed -n 's/^toolchain \(go[0-9][0-9.]*\)[[:space:]]*$/\1/p' "$src/go.mod")"
[[ -n "$version" ]] || fail "go.mod of the action has no toolchain line"

for v in $(compgen -e); do
  case "$v" in GO* | CGO_*) unset "$v" ;; esac
done
export GOFLAGS=-mod=readonly GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org \
  GOWORK=off GOENV=off CGO_ENABLED=0

work="$(mktemp -d "$tmp/hephaestus-go.XXXXXXXX")"
cleanup() {
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
export GOCACHE="$work/cache" GOMODCACHE="$work/mod"

sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

case "${RUNNER_OS:-}/${RUNNER_ARCH:-}" in
  Linux/X64) platform=linux-amd64 ext=tar.gz ;;
  Linux/ARM64) platform=linux-arm64 ext=tar.gz ;;
  macOS/X64) platform=darwin-amd64 ext=tar.gz ;;
  macOS/ARM64) platform=darwin-arm64 ext=tar.gz ;;
  Windows/X64) platform=windows-amd64 ext=zip ;;
  Windows/ARM64) platform=windows-arm64 ext=zip ;;
  *) platform="" ext="" ;;
esac

if [[ -n "$platform" ]]; then
  file="$version.$platform.$ext"
  want="$(awk -v f="$file" '$2 == f { print $1 }' "$src/action/go-toolchain.sha256")"
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || fail "no SHA-256 pinned for $file in action/go-toolchain.sha256"
  # Kept for later runs of the action in the same job. It is in a directory
  # other steps can write, so every run copies it into its own directory and
  # checks the copy before unpacking it.
  downloads="$tmp/hephaestus-go-downloads"
  mkdir -p "$downloads"
  if [[ ! -f "$downloads/$file" ]] || [[ "$(sha256 "$downloads/$file")" != "$want" ]]; then
    rm -f "$downloads/$file"
    curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$downloads/$file.part" "https://dl.google.com/go/$file"
    mv "$downloads/$file.part" "$downloads/$file"
  fi
  cp "$downloads/$file" "$work/$file"
  got="$(sha256 "$work/$file")"
  [[ "$got" == "$want" ]] || fail "$file has SHA-256 $got, want $want"
  if [[ "$ext" == zip ]]; then
    # Windows tar.exe (bsdtar) reads zip archives; Git Bash's GNU tar does not.
    "$(cygpath -u "$SYSTEMROOT")/System32/tar.exe" -xf "$(cygpath -w "$work/$file")" -C "$(cygpath -w "$work")"
  else
    tar -xzf "$work/$file" -C "$work"
  fi
  rm -f "$work/$file"
  go="$work/go/bin/go$bin_ext"
  export GOTOOLCHAIN=local
else
  go="$(command -v go)" || fail "no Go toolchain is pinned for ${RUNNER_OS:-?}/${RUNNER_ARCH:-?} and no go is on PATH; install Go 1.21 or later before this action"
  export GOTOOLCHAIN="$version"
fi

# From the action's source, so that a go.mod in the caller's directory does
# not select the toolchain.
cd "$src"
[[ "$("$go" env GOVERSION)" == "$version" ]] || fail "the Go toolchain is $("$go" env GOVERSION), want $version"

bin="$tmp/hephaestus-bin/hephaestus$bin_ext"
"$go" build -trimpath -mod=readonly -o "$bin" .
"$go" version "$bin"
echo "bin=$bin" >>"$GITHUB_OUTPUT"
