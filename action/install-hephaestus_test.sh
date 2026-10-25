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

# Archives, written member by member with Python's tarfile so that every
# platform builds exactly the crafted entries (tar itself strips ../ and
# cannot make links everywhere). Each is extracted into a new directory; the
# extraction may write the binary only.
py=""
for c in python3 python; do
  if command -v "$c" >/dev/null && "$c" -c 'import sys; sys.exit(sys.version_info < (3,))' 2>/dev/null; then py=$c; break; fi
done
# craft NAME ENTRY...: ENTRY is reg:PATH, dir:PATH or sym:PATH:TARGET.
craft() {
  local name=$1
  shift
  "$py" - "$work/$name.tar.gz" "$@" <<'PY'
import io, sys, tarfile
with tarfile.open(sys.argv[1], "w:gz") as t:
    for e in sys.argv[2:]:
        kind, _, rest = e.partition(":")
        info = tarfile.TarInfo(rest.split(":")[0])
        if kind == "reg":
            data = b"#!/bin/sh\n"
            info.size, info.mode = len(data), 0o755
            t.addfile(info, io.BytesIO(data))
        elif kind == "dir":
            info.type, info.mode = tarfile.DIRTYPE, 0o755
            t.addfile(info)
        else:
            info.type, info.linkname = tarfile.SYMTYPE, rest.split(":", 1)[1]
            t.addfile(info)
PY
}
if [[ -z "$py" ]]; then
  echo "skip crafted archives (no Python 3)"
else
  craft good reg:hephaestus reg:LICENSE reg:README.md
  expect_ok "a regular binary at the root" extract_binary "$work/good.tar.gz" hephaestus "$work/x-good"
  expect_eq "only the binary is written" "$(ls -A "$work/x-good")" hephaestus

  craft link sym:hephaestus:/etc/passwd
  expect_fail "the binary is a symlink" extract_binary "$work/link.tar.gz" hephaestus "$work/x-link"
  craft link2 reg:real sym:hephaestus:real
  expect_fail "the binary is a symlink to another member" extract_binary "$work/link2.tar.gz" hephaestus "$work/x-link2"
  craft nested dir:bin reg:bin/hephaestus
  expect_fail "the binary is not at the root" extract_binary "$work/nested.tar.gz" hephaestus "$work/x-nested"
  craft dir dir:hephaestus
  expect_fail "the binary is a directory" extract_binary "$work/dir.tar.gz" hephaestus "$work/x-dir"
  craft none reg:README.md
  expect_fail "no binary" extract_binary "$work/none.tar.gz" hephaestus "$work/x-none"
  craft dup reg:hephaestus reg:hephaestus
  expect_fail "two binaries" extract_binary "$work/dup.tar.gz" hephaestus "$work/x-dup"
  craft dotslash reg:./hephaestus
  expect_fail "the binary as ./hephaestus" extract_binary "$work/dotslash.tar.gz" hephaestus "$work/x-dotslash"
  craft trav reg:hephaestus reg:../evil reg:/tmp/hephaestus-install-test-abs
  mkdir "$work/x-trav"
  expect_ok "../ and absolute members next to the binary" extract_binary "$work/trav.tar.gz" hephaestus "$work/x-trav/in"
  expect_eq "only the binary is written" "$(ls -A "$work/x-trav") $(ls -A "$work/x-trav/in")" "in hephaestus"
  [[ -e /tmp/hephaestus-install-test-abs ]] && expect_eq "the absolute member was not written" written no
fi

# The existing directory guard: extraction needs a new directory.
mkdir "$work/exists"
expect_fail "the extraction directory exists" extract_binary "$work/good.tar.gz" hephaestus "$work/exists"

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "all install-hephaestus tests passed"
