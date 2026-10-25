#!/usr/bin/env bash
# Rewrites action/go-toolchain.sha256 for the toolchain go.mod names: the
# SHA-256 of the official Go archive for each GitHub-hosted platform, as
# published next to it on dl.google.com. The action checks every download
# against this file (action/build-hephaestus.sh). Run it after changing the
# toolchain line of go.mod, and review the diff.
set -euo pipefail
cd "$(dirname "$0")/.."
version="$(sed -n 's/^toolchain \(go[0-9][0-9.]*\)[[:space:]]*$/\1/p' go.mod)"
[[ -n "$version" ]] || { echo "go.mod has no toolchain line" >&2; exit 1; }
out="$(mktemp)"
trap 'rm -f "$out"' EXIT
for p in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64; do
  ext=tar.gz
  [[ "$p" == windows-* ]] && ext=zip
  file="$version.$p.$ext"
  sum="$(curl --proto '=https' --tlsv1.2 -fsSL "https://dl.google.com/go/$file.sha256")"
  [[ "$sum" =~ ^[0-9a-f]{64}$ ]] || { echo "unexpected checksum for $file: $sum" >&2; exit 1; }
  printf '%s  %s\n' "$sum" "$file" >>"$out"
done
cp "$out" action/go-toolchain.sha256
git diff --stat -- action/go-toolchain.sha256
