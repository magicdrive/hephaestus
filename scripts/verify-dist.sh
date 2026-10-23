#!/usr/bin/env bash
# Verifies a hephaestus release dist directory before anything is published:
# every expected archive exists, contains exactly the declared files with the
# right modes, the checksums file matches, and the host binary runs.
# Usage: scripts/verify-dist.sh <dist>
set -euo pipefail
export LC_ALL=C
dist="${1:?usage: verify-dist.sh <dist>}"
cd "$dist"

sumfile=hephaestus_checksums.txt
[[ -f "$sumfile" ]] || { echo "missing $sumfile"; exit 1; }
if command -v sha256sum >/dev/null; then sha256sum -c "$sumfile"; else shasum -a 256 -c "$sumfile"; fi

count=0
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    if [[ "$os" == windows ]]; then
      f=$(ls hephaestus_*_"${os}_${arch}".zip)
      got="$(unzip -Z1 "$f" | sort | tr '\n' ' ')"
      want="LICENSE README.md hephaestus.exe "
    else
      f=$(ls hephaestus_*_"${os}_${arch}".tar.gz)
      got="$(tar -tzf "$f" | sort | tr '\n' ' ')"
      want="LICENSE README.md hephaestus "
      tar -tvzf "$f" | grep -q -- '-rwxr-xr-x .* hephaestus$' || { echo "$f: hephaestus is not 0755"; exit 1; }
    fi
    [[ "$got" == "$want" ]] || { echo "$f: contents '$got', want '$want'"; exit 1; }
    grep -q "  $f\$" "$sumfile" || { echo "$f missing from $sumfile"; exit 1; }
    count=$((count + 1))
  done
done
[[ $count -eq 6 ]]

host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
host_arch="$(uname -m)"; [[ "$host_arch" == x86_64 ]] && host_arch=amd64; [[ "$host_arch" == aarch64 ]] && host_arch=arm64
if [[ "$host_os" == darwin || "$host_os" == linux ]]; then
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  tar -xzf hephaestus_*_"${host_os}_${host_arch}".tar.gz -C "$tmp"
  "$tmp/hephaestus" version
fi
echo "dist OK: $count archives"
