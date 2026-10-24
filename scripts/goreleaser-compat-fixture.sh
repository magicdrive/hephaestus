#!/usr/bin/env bash
# Records what a pinned, real GoReleaser produces for the compatibility project
# in testdata/compat/goreleaser/project, so Hephaestus' GoReleaser-compatible
# Homebrew output can be compared against it.
#
# Steps: download the pinned GoReleaser release asset and checksums.txt into a
# fresh directory, verify the sha256, extract it, copy the project into a
# throw-away git repository tagged v1.2.3, run `goreleaser release` with
# publishing, announcing and validation skipped, then copy the generated
# formulae, artifacts.json, metadata.json, checksums.txt and a normalized
# listing of every archive into testdata/compat/goreleaser/<version>/.
#
# Nothing is published: --skip=publish,announce,validate and a dummy token.
# Requires: gh (authenticated, for the download), go, git, tar, shasum or
# sha256sum.
set -euo pipefail
export LC_ALL=C

version="v2.19.0"
repo="$(cd "$(dirname "$0")/.." && pwd)"
project="$repo/testdata/compat/goreleaser/project"
out="$repo/testdata/compat/goreleaser/$version"
work="$(mktemp -d)"

cleanup() { rm -rf "$work"; }
trap cleanup EXIT

case "$(uname -s)/$(uname -m)" in
  Darwin/x86_64) asset="goreleaser_Darwin_x86_64.tar.gz" ;;
  Darwin/arm64) asset="goreleaser_Darwin_arm64.tar.gz" ;;
  Linux/x86_64) asset="goreleaser_Linux_x86_64.tar.gz" ;;
  Linux/aarch64 | Linux/arm64) asset="goreleaser_Linux_arm64.tar.gz" ;;
  *) echo "unsupported platform: $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Download into its own empty directory; the contents are untrusted until
# verified, and nothing is executed from there.
dl="$work/download"
mkdir "$dl"
gh release download "$version" --repo goreleaser/goreleaser \
  --pattern "$asset" --pattern checksums.txt --dir "$dl"

want="$(awk -v a="$asset" '$2 == a {print $1}' "$dl/checksums.txt")"
got="$(sha256 "$dl/$asset")"
if [[ -z "$want" || "$want" != "$got" ]]; then
  echo "checksum mismatch for $asset: want '$want', got '$got'" >&2
  exit 1
fi

bin="$work/bin"
mkdir "$bin"
tar -xzf "$dl/$asset" -C "$bin" goreleaser
goreleaser="$bin/goreleaser"
"$goreleaser" --version | grep -q "GitVersion:[[:space:]]*${version#v}\$" || {
  echo "unexpected goreleaser version" >&2
  "$goreleaser" --version >&2
  exit 1
}

# Throw-away git repository so the release gets the real version 1.2.3.
src="$work/src"
mkdir "$src"
cp -R "$project/." "$src/"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.com
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.com
export GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z"
git -C "$src" init -q -b main
git -C "$src" add -A
git -C "$src" commit -q -m "fixture"
git -C "$src" tag -a v1.2.3 -m v1.2.3
# GoReleaser requires a remote; it is never contacted (publish is skipped).
git -C "$src" remote add origin https://github.com/acme/compatcli.git

# A dummy token keeps the GitHub client happy; publishing is skipped, so it is
# never sent anywhere. Other forge tokens are removed so GoReleaser does not
# refuse to pick one.
(
  cd "$src"
  env -u GOROOT -u GITLAB_TOKEN -u GITEA_TOKEN GITHUB_TOKEN=dummy-not-used \
    "$goreleaser" release --clean --parallelism=1 --skip=publish,announce,validate
)

dist="$src/dist"
rm -rf "$out"
mkdir -p "$out/homebrew" "$out/archives"
cp "$dist"/homebrew/*.rb "$out/homebrew/"
cp "$dist/artifacts.json" "$dist/metadata.json" "$dist/checksums.txt" "$out/"

# Archive contents: mode and path only, sorted, one file per archive.
for archive in "$dist"/*.tar.gz; do
  name="$(basename "$archive")"
  tar -tvzf "$archive" | awk '{print $1, $NF}' | sort -k2 >"$out/archives/$name.list"
done

echo "fixtures written to $out"
