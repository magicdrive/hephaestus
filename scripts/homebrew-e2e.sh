#!/usr/bin/env bash
# Homebrew end-to-end test.
#
# Builds the fixture CLI for four targets with Hephaestus, archives it, checks
# that archives are reproducible and laid out as declared, generates a formula
# pointing at the local archives (file:// URLs), then runs the real Homebrew:
# brew audit --strict, brew style on every golden formula, brew install,
# brew test, the installed executable and its man page, and brew uninstall,
# inside throw-away taps.
#
# It installs into the machine's Homebrew prefix and removes everything it
# created on exit. Intended for disposable CI runners; safe but noticeable on a
# workstation.
set -euo pipefail
export LC_ALL=C

repo="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$repo/testdata/e2e/fixture"
work="$(mktemp -d)"
tag="v0.1.$(date +%s)"
tap="hephaestus-e2e/t$RANDOM$RANDOM"
formula="heph-fixture"

export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_DEVELOPER=1 HOMEBREW_NO_ASK=1 \
  HOMEBREW_NO_INSTALL_CLEANUP=1 HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1 \
  HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_ENV_HINTS=1

cleanup() {
  set +e
  brew uninstall --force "$tap/$formula" >/dev/null 2>&1
  brew untap --force "$tap" >/dev/null 2>&1
  brew untap --force "${tap}style" >/dev/null 2>&1
  rm -rf "$work" "$fixture/dist"
}
trap cleanup EXIT

step() { printf '\n==> %s\n' "$*"; }

step "Build hephaestus"
(cd "$repo" && go build -trimpath -o "$work/hephaestus" .)
heph="$work/hephaestus"

step "Forge fixture $tag: build, archive, checksum, formula"
cd "$fixture"
rm -rf dist
export E2E_URL_BASE="file://$fixture/dist" HEPHAESTUS_ALLOW_FILE_URLS=1
"$heph" run --stages build,archive,checksum,formula --tag "$tag" --fresh
version="${tag#v}"

step "Archives are laid out as declared"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"; [[ "$arch" == x86_64 ]] && arch=amd64; [[ "$arch" == aarch64 ]] && arch=arm64
archive="dist/heph-fixture_${version}_${os}_${arch}.tar.gz"
listing="$(tar -tzf "$archive" | sort)"
expected="$(printf '%s\n' LICENSE README.md bin/ bin/heph-fixture share/ share/man/ share/man/man1/ share/man/man1/heph-fixture.1 | sort)"
if [[ "$listing" != "$expected" ]]; then
  echo "unexpected archive contents:"; echo "$listing"; exit 1
fi
# Capture the listing first: grep -q exits at the first match, and tar's
# write error on the closed pipe would fail the script under pipefail.
verbose="$(tar -tvzf "$archive")"
grep -q -- '-rwxr-xr-x .*bin/heph-fixture$' <<<"$verbose"

step "Archives are byte-for-byte reproducible"
cp "dist/heph-fixture_${version}_checksums.txt" "$work/first.txt"
"$heph" run --stages archive,checksum --tag "$tag"
diff -u "$work/first.txt" "dist/heph-fixture_${version}_checksums.txt"
(cd dist && shasum -a 256 -c "heph-fixture_${version}_checksums.txt")

step "Create throw-away tap $tap"
brew tap-new --no-git "$tap" >/dev/null
tapdir="$(brew --repository "$tap")"
cp "dist/homebrew/$formula.rb" "$tapdir/Formula/$formula.rb"
cat "$tapdir/Formula/$formula.rb"

step "brew audit --strict"
brew audit --strict --formula "$tap/$formula"

step "brew style on the generator's golden formulas"
# Each golden is checked alone, as mycli.rb in a second throw-away tap. The
# custom golden keeps def post_install on purpose (deprecated, warned).
style_tap="${tap}style"
brew tap-new --no-git "$style_tap" >/dev/null
style_dir="$(brew --repository "$style_tap")/Formula"
for golden in "$repo"/internal/homebrew/testdata/*.golden; do
  rm -f "$style_dir"/*.rb
  cp "$golden" "$style_dir/mycli.rb"
  except=()
  [[ "$(basename "$golden")" == custom.golden ]] && except=(--except-cops FormulaAudit/InstallSteps)
  basename "$golden"
  # ${a[@]+...}: an empty array is "unbound" under set -u in bash 3.2.
  brew style ${except[@]+"${except[@]}"} "$style_tap"
done
brew untap --force "$style_tap" >/dev/null

step "brew install"
brew install --formula "$tap/$formula"

step "brew test"
brew test "$tap/$formula"

step "Installed executable and man page"
prefix="$(brew --prefix)"
out="$("$prefix/bin/heph-fixture" --version)"
[[ "$out" == "heph-fixture $version" ]] || { echo "unexpected output: $out"; exit 1; }
[[ -f "$(brew --prefix "$tap/$formula")/share/man/man1/heph-fixture.1" ]]

step "brew uninstall"
brew uninstall "$tap/$formula"

echo
echo "Homebrew E2E passed for $tag on $os/$arch"
