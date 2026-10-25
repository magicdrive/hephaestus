# Changelog

All notable changes to Hephaestus. The notes of each release are published
from `RELEASE_NOTES.md` (`release.github.notes_file`).

## Unreleased

### Fixed

- `run --snapshot` skips the formula stage too (it needs a tag): with
  `brews` configured, which every `init` template has, `run --snapshot`
  failed with "a tag is required". Naming formula with `--snapshot` is
  refused like release and tap.
- The `init` templates' formula test is `system bin/"NAME", "--version"`;
  `"#{bin}/NAME"` failed `brew audit --strict`.
- `make publish` re-reads the release job's tap steps when GitHub reports
  them only partially right after the run, instead of failing the
  verification of a correct release (seen on v1.0.0-rc.1, verified with
  `make publish-watch`).

### Changed (release safety)

- Hephaestus never deletes a release. A run whose new draft loses a race
  for the tag (another run's lower-id draft, or a published release) now
  stops with a conflict and leaves its empty draft for the owner, instead
  of deleting it: once listed, another run could already be using it.
- The draft is re-read right before it is published: a release published
  meanwhile is not edited, and a draft whose assets changed (added,
  removed, incomplete or different) or that lost to another release is not
  published.
- A published release's assets are never replaced or deleted, on mutable
  releases too: `replace_existing_assets` now applies to drafts only, and
  an incomplete asset on a published release is reported, not removed.
- An incomplete asset seen right after a failed upload is deleted only when
  it is older than the 10-minute grace period: a recent one cannot be told
  apart from another run's upload in progress.

## v1.0.0-rc.1

First release candidate of 1.0; full notes in
[RELEASE_NOTES.md](RELEASE_NOTES.md).

### Added

- **Release safety**: `preflight` in the GitHub Action (`strict`,
  `offline`; `preflight-status`, `preflight-exit-code`, `preflight-report`
  outputs; annotations; redacted summary); check `config.release-notes`
  (notes file and optional `preflight.changelog` must name the tag);
  `preflight --report FILE`; `make publish` gated on `preflight --strict`.
- **Noarch archives** (`targets: [{os: all, arch: all}]`): one archive and
  one formula `url` for every platform.
- **Formula verification** rejects installs that cannot all succeed: a
  file installed twice, a directory and a file inside it, two installs to
  one destination.
- **`hephaestus status --tag`**: read-only state of a release (tag,
  release, assets by digest, tap pull requests) and the next safe action;
  [docs/recovery.md](docs/recovery.md).
- **`hephaestus init --template go|rust|script|prebuilt`**.
- Full-pipeline E2E and recovery tests against a fake GitHub; CI on Linux
  arm64 for every change, macOS Intel weekly.
- [Compatibility policy](docs/compatibility/policy.md) and
  [CLI reference](docs/cli.md).

### Changed

- A published release (mutable or immutable) is never edited: title,
  notes and prerelease flag are set on the draft; a re-run that would
  change them logs a warning. v0.2.0 updated them.
- The release workflow stops a prerelease whose plan would publish the
  tap.

### Compatibility

- v0.2.0 configurations load unchanged (`testdata/compat/v0.2.0`).
  Configuration keys, CLI commands and flags, Action inputs and outputs,
  JSON fields and check IDs were only added.
- Formula verification can now reject a configuration whose formula could
  never install (a file installed twice).
- `brews[].skip_upload` still defaults to `false`; set `auto` to keep
  prereleases out of the tap.

### Limitations

- Windows arm64 is built but not tested; the Rust template needs the Rust
  targets and linkers installed; recovery is tested against a fake GitHub
  API, not by injecting failures into the real one.

## v0.2.0

See [RELEASE_NOTES.md](RELEASE_NOTES.md) (the notes of this release):
`hephaestus preflight`, deterministic check-run evaluation, read-only GitHub
API additions, shared archive and tap rules.

## v0.1.0

First stable release: build, archive, checksum, GitHub release
(draft-first, idempotent, immutable-release ready), provenance
attestation, Homebrew formula generation and tap publication, the
composite GitHub Action, and `make publish` for releasing Hephaestus itself.
