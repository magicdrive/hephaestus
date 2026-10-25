#!/usr/bin/env bash
# Tests of action/install-hephaestus.sh that need no network: the reference
# and platform rules, the checksum check and the extraction of the binary
# from crafted archives (links, traversal, duplicates, directories).
# The download and attestation path is tested against a real release by
# the action-install job in ci.yml.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=action/install-hephaestus.sh
source "$here/install-hephaestus.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failures=0

# expect_fail NAME CMD...: CMD must exit non-zero (fail runs exit 1).
expect_fail() {
  local name=$1
  shift
  if ("$@") >"$work/out" 2>&1; then
    echo "FAIL $name: succeeded"
    failures=$((failures + 1))
  else
    echo "ok   $name: $(tail -1 "$work/out")"
  fi
}
expect_ok() {
  local name=$1
  shift
  if ("$@") >"$work/out" 2>&1; then
    echo "ok   $name"
  else
    echo "FAIL $name: $(cat "$work/out")"
    failures=$((failures + 1))
  fi
}
expect_eq() {
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else
    echo "FAIL $1: got '$2', want '$3'"
    failures=$((failures + 1))
  fi
}

sha=0123456789abcdef0123456789abcdef01234567
actions="$work/_actions/magicdrive/hephaestus"

# References.
expect_eq "full SHA from github.action_ref" "$(source_digest "$sha" v1.2.0 /x)" "$sha"
expect_eq "release tag" "$(source_digest v1.2.0 v1.2.0 /x)" ""
expect_eq "SHA from the action path" "$(source_digest "" v1.2.0 "$actions/$sha")" "$sha"
expect_eq "tag from the action path" "$(source_digest "" v1.2.0 "$actions/v1.2.0")" ""
expect_fail "branch reference" source_digest main v1.2.0 /x
expect_fail "another release's tag" source_digest v1.1.0 v1.2.0 /x
expect_fail "short SHA" source_digest 0123456 v1.2.0 /x
expect_fail "upper-case SHA" source_digest "$(tr a-f A-F <<<"$sha")" v1.2.0 /x
expect_fail "another repository's path" source_digest "" v1.2.0 "$work/_actions/someone/hephaestus/$sha"

# Platforms.
expect_eq "Linux X64" "$(platform Linux X64)" "linux_amd64 tar.gz"
expect_eq "macOS ARM64" "$(platform macOS ARM64)" "darwin_arm64 tar.gz"
expect_eq "Windows ARM64" "$(platform Windows ARM64)" "windows_arm64 zip"
expect_fail "unsupported platform" platform Linux X86

# Checksums.
printf 'binary\n' >"$work/a.tar.gz"
good="$(sha256 "$work/a.tar.gz")"
printf '%s  a.tar.gz\n%s  b.tar.gz\n' "$good" "$(printf '0%.0s' {1..64})" >"$work/sums"
expect_ok "checksum matches" verify_checksum "$work/a.tar.gz" "$work/sums" a.tar.gz
printf '%s  a.tar.gz\n' "$(printf 'f%.0s' {1..64})" >"$work/bad"
expect_fail "checksum mismatch" verify_checksum "$work/a.tar.gz" "$work/bad" a.tar.gz
expect_fail "no checksum line" verify_checksum "$work/a.tar.gz" "$work/sums" c.tar.gz
printf '%s  a.tar.gz\n%s  a.tar.gz\n' "$good" "$good" >"$work/dup"
expect_fail "two checksum lines" verify_checksum "$work/a.tar.gz" "$work/dup" a.tar.gz

# Archives. Each is built from a directory and extracted into a new one;
# the extraction must write the binary only.
make_archive() { # NAME SETUP...: runs SETUP in a new directory, then archives it
  local name=$1 d="$work/src-$1"
  shift
  mkdir "$d"
  (cd "$d" && eval "$*")
  # shellcheck disable=SC2046 # the member names are simple words
  tar -czf "$work/$name.tar.gz" -C "$d" $(cd "$d" && ls -A)
}
make_archive good 'printf "#!/bin/sh\n" >hephaestus; chmod 755 hephaestus; printf x >LICENSE; printf x >README.md'
expect_ok "a regular binary at the root" extract_binary "$work/good.tar.gz" hephaestus "$work/x-good"
expect_eq "only the binary is written" "$(ls -A "$work/x-good")" hephaestus

make_archive link 'ln -s /etc/passwd hephaestus'
expect_fail "the binary is a symlink" extract_binary "$work/link.tar.gz" hephaestus "$work/x-link"

make_archive nested 'mkdir bin; printf x >bin/hephaestus'
expect_fail "the binary is not at the root" extract_binary "$work/nested.tar.gz" hephaestus "$work/x-nested"

make_archive dir 'mkdir hephaestus'
expect_fail "the binary is a directory" extract_binary "$work/dir.tar.gz" hephaestus "$work/x-dir"

make_archive none 'printf x >README.md'
expect_fail "no binary" extract_binary "$work/none.tar.gz" hephaestus "$work/x-none"

# Two members with the same name.
mkdir -p "$work/dup1" "$work/dup2"
printf one >"$work/dup1/hephaestus"
printf two >"$work/dup2/hephaestus"
tar -cf "$work/dup.tar" -C "$work/dup1" hephaestus
tar -rf "$work/dup.tar" -C "$work/dup2" hephaestus
gzip -c "$work/dup.tar" >"$work/dup.tar.gz"
expect_fail "two binaries" extract_binary "$work/dup.tar.gz" hephaestus "$work/x-dup"

# A traversal member next to the binary is never extracted. GNU tar can
# write such an entry with --transform; elsewhere the case is skipped.
mkdir -p "$work/trav"
printf x >"$work/trav/hephaestus"
printf evil >"$work/trav/evil"
if tar --version 2>/dev/null | grep -q 'GNU tar' &&
  tar -czf "$work/trav.tar.gz" -C "$work/trav" --transform 's,^evil$,../evil,' hephaestus evil 2>/dev/null; then
  expect_ok "a ../ member next to the binary" extract_binary "$work/trav.tar.gz" hephaestus "$work/x-trav/in"
  expect_eq "the ../ member was not written" "$(ls -A "$work/x-trav")" in
else
  echo "skip a ../ member next to the binary (needs GNU tar to build the archive)"
fi

# The existing directory guard: extraction needs a new directory.
mkdir "$work/exists"
expect_fail "the extraction directory exists" extract_binary "$work/good.tar.gz" hephaestus "$work/exists"

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "all install-hephaestus tests passed"
