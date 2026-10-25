# Hephaestus v1.0.0

Hephaestus 1.0 is release automation for command-line tools written in any
language. From one `hephaestus.yaml` it turns your executables into
reproducible archives, a checksum file and a GitHub Release, then delivers
a Homebrew formula to your tap as a pull request. You choose how the
executables are made: Hephaestus runs your build command per target, or
imports binaries that are already built. It is a single Go binary and a
composite GitHub Action.

1.0 is about releasing without surprises:

- **Check before you tag.** `hephaestus preflight` checks the repository,
  the configuration, the release notes and GitHub (rulesets, environment,
  CI, existing releases) and tells you what would block the release.
- **Never half publish.** A release is created as a draft. It is published
  only after every asset has been uploaded and verified against the local
  build.
- **Never destroy what is already public.** A published release is never
  edited, its assets are never replaced or deleted, and Hephaestus never
  deletes a release.
- **Know where you stand after a failure.** `hephaestus status` reads, and
  never writes, what a release run left behind and names the next safe
  action. Re-running resumes from that point.

From 1.0.0 Hephaestus follows Semantic Versioning, with the stable surface
defined in the
[compatibility policy](https://github.com/magicdrive/hephaestus/blob/v1.0.0/docs/compatibility/policy.md).

## Key features

### Release pipeline

- **Build**: your own command per target, or prebuilt binaries imported from
  a path template.
- **Archive**: `tar.gz` and `zip` archives with an explicit file list. They
  are deterministic: fixed timestamps, order and modes, so the same inputs
  give the same bytes. A noarch archive (`targets: [{os: all, arch: all}]`)
  is one archive for every platform, for scripts.
- **Checksum**: one SHA-256 file over the archives.
- **GitHub Release**: draft first, idempotent and compatible with immutable
  releases. Every asset is compared by digest; `make_latest` and
  `prerelease: auto` are supported.
- **Homebrew formula**: generated from the manifest of what was built,
  uploaded and checksummed, in current Homebrew style (`on_macos`,
  `on_linux`, `on_arm`, `on_intel`), or a single `url` for a noarch archive.
- **Tap pull request**: the formula is delivered to your tap as a pull
  request, never by force-push. Prereleases can skip the tap
  (`skip_upload: auto`).

### Safety

- **Strict preflight**: `hephaestus preflight --tag vX.Y.Z --strict` runs 26
  read-only checks. It is also available in the Action (`command:
  preflight`), with a JSON report.
- **Release notes validation**: the notes file and the changelog must name
  the tag as a whole version. `v1.2.0` is not satisfied by `v1.2.0-rc.1`.
- **Published releases are left as published**: a published release's
  title, notes and prerelease flag are never edited; a difference is only
  reported. Its assets are never replaced or deleted, on mutable releases
  too.
- **Conflict detection**: an asset that differs from the local build stops
  the run. So does a tag that points to another commit, or a release for the
  same tag created by a concurrent run; nothing is overwritten or deleted.
- **Re-reading before every write**: a draft is read again right before it
  is published, before one of its assets is deleted, and before its title or
  notes are updated. A release that was published in the meantime is left
  alone.
- **Safe recovery**: a lost API response is followed by a lookup, never by
  a blind second create, and an interrupted run resumes from where it
  stopped. The [recovery guide](https://github.com/magicdrive/hephaestus/blob/v1.0.0/docs/recovery.md)
  covers 12 failure scenarios.
- **Read-only status**: `hephaestus status --tag vX.Y.Z` reports one of
  `not-started`, `tagged`, `draft`, `published-incomplete`, `conflict`,
  `published` or `completed`. It compares assets by digest and finds each
  formula's tap pull request. Use `--format json` for scripts.
- **Token hygiene**: publishing tokens are removed from the build's
  environment and redacted from every output.

### Artifact quality

- **Reproducible**: rebuilding the v1.0.0-rc.2 tag from a clean checkout
  reproduced all of its published archives byte for byte.
- **Checksums and attestations**: every archive and the checksum file carry
  a build provenance attestation from the release workflow
  (`gh attestation verify <file> --repo magicdrive/hephaestus`).
- **Formula install collision detection**: a formula that would install
  the same file twice, a directory and a file inside it, or two files to
  one destination is rejected when it is generated. Such a formula could
  never install.

### Developer experience

- **`hephaestus init --template go|rust|script|prebuilt`** writes a
  starting `hephaestus.yaml` that passes `check` and `run --snapshot`, and
  whose formula passes `brew audit --strict`. For Rust, `run --snapshot`
  needs the template's Rust targets installed. Prereleases skip the tap.
  `init` never overwrites a file.
- **`run --snapshot`** builds archives and checksums without a tag. Release,
  formula and tap stages need a tag and are skipped.
- **Composite GitHub Action**: builds Hephaestus from the commit you pin,
  and runs `run`, `check`, any single stage, or `preflight`.
- **Documentation**: a [CLI reference](https://github.com/magicdrive/hephaestus/blob/v1.0.0/docs/cli.md),
  configuration, archives, the GitHub Action, recovery, GoReleaser
  compatibility and migration.

## Supported platforms

Release archives are published for six platforms. What was verified differs
by platform:

| Platform | Archive | Tests from source in CI | Published binary run (v1.0.0-rc.2) |
| --- | --- | --- | --- |
| macOS amd64 | yes | weekly extended CI (`macos-15-intel`) | yes: `verify-release` workflow; full local dogfooding (templates, Homebrew install) |
| macOS arm64 | yes | every change (`macos-latest`) | yes: `verify-release` workflow |
| Linux amd64 | yes | every change | yes: `verify-release` workflow; container |
| Linux arm64 | yes | every change (`ubuntu-24.04-arm`) | yes: `verify-release` workflow; container |
| Windows amd64 | yes | every change | yes: `verify-release` workflow |
| Windows arm64 | yes | — | yes: `verify-release` workflow (`windows-11-arm`) |

"Published binary run" means the archive was downloaded from the release
and checked against the checksum file, then `version`, `--help`, `init` and
`check` were run. Generated formulas are installed and tested with Homebrew
on macOS and Linux in CI.

## Compatibility with v0.2.0

- **Configuration**: v0.2.0 configurations load unchanged; Hephaestus's own
  and magicdrive/t's are kept as regression fixtures.
- **CLI, Action, check IDs, JSON**: changes are additions only:
  - commands `status` and `init`;
  - `preflight --report`;
  - Action inputs `strict` and `offline`, and outputs `preflight-status`,
    `preflight-exit-code` and `preflight-report`;
  - the check `config.release-notes`;
  - configuration `preflight.changelog` and noarch targets.

  Nothing was removed or renamed. The preflight JSON report keeps
  `schema_version: 1`.

Some behaviors changed, each to protect what is already published, or to
reject something that could never have worked:

- **A published release's title, notes and prerelease flag are never
  edited.** v0.2.0 updated them on a re-run. A difference is now a warning,
  and the formula and tap can still be resumed.
- **A published release's assets are never replaced or deleted.**
  `replace_existing_assets` now applies to drafts only. v0.2.0 replaced a
  differing asset of a published mutable release when it was set. An
  incomplete asset on a published release is reported, not removed.
- **A draft that lost a concurrent race is not deleted.** When two runs
  race for a tag, the losing run stops with a conflict and leaves its empty
  draft for you to delete.
- **`run --snapshot` does not run the formula stage**, just as it already
  skipped release and tap: a formula needs a tag. Naming formula, release or
  tap in `--stages` together with `--snapshot` is an error.
- **A formula that installs the same file twice is rejected** when it is
  generated, as are the other install collisions above.

## Known limitations

- **Rust cross-builds**: the Rust template builds each target with `cargo
  build --target`, so every target needs its Rust target and a linker
  installed. Alternatively, build on one runner per platform and import the
  binaries as prebuilt. The template was verified for the host target
  (x86_64-apple-darwin) only; its other targets were not built.
- **Recovery is tested against a fake GitHub API.** Failures were injected
  there: lost responses, concurrent runs, digest conflicts and failed tap
  pull requests. Against the real GitHub API, the normal release path is
  exercised by every release.
- **No conditional updates on GitHub**: Hephaestus re-reads a release right
  before each write. Another writer can still act in the single API call
  between that read and the write. The release workflow serializes runs per
  tag (`concurrency`), which avoids this in normal use.
- **Incomplete uploads wait**: if a failed upload leaves an incomplete
  asset, that asset is deleted only when it is more than 10 minutes old, so
  that an upload by another run in progress is never removed. A re-run after
  that time continues.
- **Prereleases and the tap**: `brews[].skip_upload` defaults to `false`, so
  a prerelease is published to the tap unless you set `skip_upload: auto`
  (as `init` does) or `true`. The default changes only in a major release.

## Installation

- **Homebrew**: when this release is published, the 1.0.0 formula goes to
  [magicdrive/homebrew-tap](https://github.com/magicdrive/homebrew-tap) as
  a pull request, which is merged after review. From then on,
  `brew install magicdrive/tap/hephaestus` installs 1.0.0, and
  `brew upgrade hephaestus` upgrades from 0.2.0. Until the pull request is
  merged, the tap installs the previous version.
- **Archives**: download the archive for your platform from this release,
  and check it with `hephaestus_checksums.txt` and
  `gh attestation verify <file> --repo magicdrive/hephaestus`.
- **Go**: `go install github.com/magicdrive/hephaestus@v1.0.0` (Go 1.26 or
  later).
- **GitHub Actions**: `uses: magicdrive/hephaestus@<full-commit-sha>`, with
  the commit of this tag.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v0.2.0...v1.0.0
