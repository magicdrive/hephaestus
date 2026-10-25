#!/usr/bin/env bash
# Installs the hephaestus binary for the composite action (action.yml): the
# release binary of the version in action/VERSION, downloaded from that
# GitHub Release and verified before it runs. Nothing is compiled, no Go
# toolchain is downloaded, and nothing is written to $GITHUB_PATH or
# $GITHUB_ENV: the caller's Go, Rust, shell, PATH and environment stay as
# they are.
#
# Binding between the action's source and the binary:
#
#   - action/VERSION names the release (vX.Y.Z or vX.Y.Z-rc.N). The release
#     workflow refuses to publish a tag that differs from it, so the commit a
#     release tag points to always names that release.
#   - The action must be referenced by that tag or by a full commit SHA. A
#     SHA must be the commit the release was built from: the provenance
#     attestation of the archive records the source commit, the tag ref and
#     the workflow that built it (magicdrive/hephaestus release.yml on a
#     GitHub-hosted runner), and gh attestation verify checks all of them.
#     Branch references fail.
#   - The archive's SHA-256 must match the release's checksum file, only the
#     regular file hephaestus[.exe] is extracted from it, and the extracted
#     binary must report the expected version.
#
# Any failure stops the action (fail closed); there is no fallback.
#
# Hephaestus's own CI runs the action from its checkout (uses: ./), whose
# version has not been released yet. Only then, when the action directory is
# the workspace itself, HEPHAESTUS_ACTION_BINARY may name a binary the
# workflow built from that checkout; it is ignored for every other caller.
#
# Environment: ACTION_PATH, ACTION_REF (github.action_ref, may be empty),
# GH_TOKEN (read-only, for the attestation API), RUNNER_OS, RUNNER_ARCH,
# RUNNER_TEMP, GITHUB_WORKSPACE, GITHUB_OUTPUT. Writes bin=<path> to
# $GITHUB_OUTPUT.
set -euo pipefail

readonly repo=magicdrive/hephaestus
readonly signer="$repo/.github/workflows/release.yml"

fail() {
  echo "::error::hephaestus action: $*" >&2
  exit 1
}

sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# source_digest REF VERSION SRC: prints the commit SHA the release binary
# must have been built from (empty when the action is referenced by the
# release tag itself), or fails for any other reference.
source_digest() {
  local ref=$1 version=$2 src=$3
  if [[ -z "$ref" ]]; then
    # github.action_ref is empty inside some composite actions; the runner
    # checks the action out under _actions/<owner>/<repo>/<ref>.
    ref="$(basename "$src")"
    case "$(basename "$(dirname "$src")")/$(basename "$(dirname "$(dirname "$src")")")" in
      hephaestus/magicdrive) ;;
      *) fail "cannot tell which commit of $repo this action is (path $src); reference it as $repo@<full commit SHA> or @$version" ;;
    esac
  fi
  if [[ "$ref" =~ ^[0-9a-f]{40}$ ]]; then
    echo "$ref"
  elif [[ "$ref" != "$version" ]]; then
    fail "the action is referenced as @$ref; reference the release it belongs to: $repo@<full commit SHA of $version> (recommended) or @$version"
  fi
}

# platform OS ARCH: prints "<os>_<arch> <extension>" of the release archive.
platform() {
  case "$1/$2" in
    Linux/X64) echo "linux_amd64 tar.gz" ;;
    Linux/ARM64) echo "linux_arm64 tar.gz" ;;
    macOS/X64) echo "darwin_amd64 tar.gz" ;;
    macOS/ARM64) echo "darwin_arm64 tar.gz" ;;
    Windows/X64) echo "windows_amd64 zip" ;;
    Windows/ARM64) echo "windows_arm64 zip" ;;
    *) fail "no Hephaestus release binary for ${1:-?}/${2:-?}; supported: Linux, macOS and Windows on X64 and ARM64" ;;
  esac
}

# verify_checksum FILE SUMS NAME: FILE's SHA-256 must be the one line for
# NAME in the checksum file SUMS.
verify_checksum() {
  local file=$1 sums=$2 name=$3 want got
  want="$(awk -v f="$name" '$2 == f || $2 == "*" f { print $1 }' "$sums" | tr -d '\r')"
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || fail "$(basename "$sums") has no single SHA-256 for $name"
  got="$(sha256 "$file")"
  [[ "$got" == "$want" ]] || fail "$name has SHA-256 $got, $(basename "$sums") says $want"
}

# extract_binary ARCHIVE MEMBER DIR: extracts the one regular file MEMBER at
# the root of ARCHIVE into the new directory DIR. No other member, path,
# directory, link or device reaches the disk. On Windows (ZIP_TAR set to
# tar.exe, bsdtar) zip archives are read; elsewhere tar.gz.
extract_binary() {
  local archive=$1 member=$2 dir=$3 entry
  local list=(tar -tvzf "$archive") extract=(tar -xzf "$archive" -C "$dir" "$member")
  if [[ -n "${ZIP_TAR:-}" ]]; then
    list=("$ZIP_TAR" -tvf "$(cygpath -w "$archive")")
    extract=("$ZIP_TAR" -xf "$(cygpath -w "$archive")" -C "$(cygpath -w "$dir")" "$member")
  fi
  entry="$("${list[@]}" | tr -d '\r' | awk -v m="$member" '$NF == m')" ||
    fail "cannot list $(basename "$archive")"
  [[ -n "$entry" && "$(wc -l <<<"$entry")" -eq 1 ]] || fail "$(basename "$archive") does not hold exactly one $member at its root"
  [[ "$entry" == -* ]] || fail "$member in $(basename "$archive") is not a regular file: $entry"
  mkdir "$dir" || fail "cannot create $dir"
  "${extract[@]}" || fail "cannot extract $member from $(basename "$archive")"
  [[ -f "$dir/$member" && ! -L "$dir/$member" ]] || fail "$member was not extracted as a regular file"
  [[ "$(find "$dir" -mindepth 1 | wc -l | tr -d ' ')" == 1 ]] || fail "extracting $member wrote other files"
}

check_version() {
  local got
  got="$("$1" version | tr -d '\r')" || fail "$1 does not run on this runner"
  [[ "$got" == "$2" ]] || fail "the binary reports \"$got\", want \"$2\""
}

canonical() { (cd "$1" 2>/dev/null && pwd -P); }

main() {
  : "${ACTION_PATH:?ACTION_PATH is not set}" "${RUNNER_TEMP:?RUNNER_TEMP is not set}" "${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set}"
  local tmp="$RUNNER_TEMP" src="$ACTION_PATH" workspace="${GITHUB_WORKSPACE:-}" bin_ext=""
  if [[ "${RUNNER_OS:-}" == "Windows" ]]; then
    # Git Bash tools and native programs both accept D:/a/_temp; RUNNER_TEMP
    # arrives as D:\a\_temp.
    tmp="$(cygpath -m "$RUNNER_TEMP")" src="$(cygpath -m "$ACTION_PATH")" bin_ext=".exe"
    [[ -n "$workspace" ]] && workspace="$(cygpath -m "$workspace")"
    # Windows tar.exe (bsdtar) reads zip archives; Git Bash's GNU tar does not.
    ZIP_TAR="$(cygpath -u "$SYSTEMROOT")/System32/tar.exe"
  fi

  local version
  version="$(tr -d '\r\n' <"$src/action/VERSION")"
  [[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] ||
    fail "action/VERSION is not a release tag: $version"

  local bin_dir="$tmp/hephaestus-bin"
  local bin="$bin_dir/hephaestus$bin_ext"
  mkdir -p "$bin_dir"
  rm -f "$bin"

  # Hephaestus's own checkout (uses: ./).
  if [[ -n "$workspace" && "$(canonical "$src")" == "$(canonical "$workspace")" ]]; then
    local dev="${HEPHAESTUS_ACTION_BINARY:-}"
    [[ -n "$dev" ]] || fail "the action runs from this checkout (uses: ./), whose $version is not released: build hephaestus first and set HEPHAESTUS_ACTION_BINARY to it"
    [[ -f "$dev" && ! -L "$dev" ]] || fail "HEPHAESTUS_ACTION_BINARY is not a regular file: $dev"
    echo "::notice::hephaestus action: running from this checkout with HEPHAESTUS_ACTION_BINARY (development of Hephaestus itself); no release binary is used"
    cp "$dev" "$bin"
    chmod 0755 "$bin"
    "$bin" version
    echo "bin=$bin" >>"$GITHUB_OUTPUT"
    return 0
  fi

  # The release binary.
  # A failure inside $(...) exits only the subshell: check each status.
  local digest target plat ext
  digest="$(source_digest "${ACTION_REF:-}" "$version" "$src")" || exit 1
  target="$(platform "${RUNNER_OS:-}" "${RUNNER_ARCH:-}")" || exit 1
  read -r plat ext <<<"$target"
  command -v gh >/dev/null || fail "the GitHub CLI (gh) is required to verify the release binary's attestation; it is preinstalled on GitHub-hosted runners"

  local asset="hephaestus_${version#v}_$plat.$ext" sums="hephaestus_checksums.txt"
  local base="https://github.com/$repo/releases/download/$version" work
  work="$(mktemp -d "$tmp/hephaestus-install.XXXXXXXX")"
  # shellcheck disable=SC2064 # expand now: work is local
  trap "rm -rf '$work'" EXIT
  local f
  for f in "$asset" "$sums"; do
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 --max-redirs 5 -fsSL --retry 3 \
      -o "$work/$f" "$base/$f" || fail "cannot download $base/$f"
  done

  verify_checksum "$work/$asset" "$work/$sums" "$asset"
  local attest=(attestation verify "$work/$asset" --repo "$repo" --signer-workflow "$signer"
    --source-ref "refs/tags/$version" --deny-self-hosted-runners) out
  [[ -n "$digest" ]] && attest+=(--source-digest "$digest")
  if ! out="$(gh "${attest[@]}" 2>&1)"; then
    echo "$out" >&2
    fail "the provenance attestation of $asset does not match $repo $version${digest:+ at $digest} built by release.yml"
  fi
  echo "verified: $asset (SHA-256 $(sha256 "$work/$asset")), attestation of $signer at refs/tags/$version${digest:+, commit $digest}"

  extract_binary "$work/$asset" "hephaestus$bin_ext" "$work/x"
  cp "$work/x/hephaestus$bin_ext" "$bin"
  chmod 0755 "$bin"
  check_version "$bin" "hephaestus $version"
  "$bin" version
  echo "bin=$bin" >>"$GITHUB_OUTPUT"
}

# Sourced by action/install-hephaestus_test.sh for its functions only.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main
fi
