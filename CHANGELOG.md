# Changelog

All notable changes to Hephaestus. The notes of each release are published
from `RELEASE_NOTES.md` (`release.github.notes_file`).

## Unreleased (v1.0.0)

- **Release safety**: `preflight` in the GitHub Action (`strict`, `offline`,
  `preflight-status`/`-exit-code`/`-report` outputs, annotations, redacted
  summary); new check `config.release-notes` (notes file and optional
  `preflight.changelog` must name the tag); `--report FILE`.
- **Noarch archives** (`targets: [{os: all, arch: all}]`): one archive and
  one formula `url` for every platform.
- **Formula verification** rejects installs that cannot all succeed: a file
  installed twice, a directory and a file inside it, two installs to one
  destination.
- **`hephaestus status --tag`**: read-only state of a release (tag, release,
  assets by digest, tap pull requests) and the next safe action;
  [docs/recovery.md](docs/recovery.md).
- **`hephaestus init --template go|rust|script|prebuilt`**.
- CI on Linux arm64 for every change; macOS Intel weekly.
- [Compatibility policy](docs/compatibility/policy.md) and
  [CLI reference](docs/cli.md).

Compatibility: v0.2.0 configurations load unchanged
(`testdata/compat/v0.2.0`). Formula verification can now reject a
configuration whose formula could never install (a file installed twice).

## v0.2.0

See [RELEASE_NOTES.md](RELEASE_NOTES.md) (the notes of this release):
`hephaestus preflight`, deterministic check-run evaluation, read-only GitHub
API additions, shared archive and tap rules.

## v0.1.0

First stable release: build, archive, checksum, GitHub release
(draft-first, idempotent, immutable-release ready), provenance
attestation, Homebrew formula generation and tap publication, the
composite GitHub Action, and `make publish` for releasing Hephaestus itself.
