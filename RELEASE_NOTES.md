# Hephaestus v1.0.0-rc.1

The first release candidate of Hephaestus 1.0. It completes the release
safety story (check before tagging, verify while releasing, inspect and
resume after a failure) and defines what 1.0 keeps stable. A prerelease:
it is not marked latest and does not update the Homebrew tap; v0.2.0
stays the latest stable release.

## Added

- **Preflight in the GitHub Action**: `command: preflight` runs
  `hephaestus preflight` for the tag. Inputs `strict` and `offline`; outputs
  `preflight-status`, `preflight-exit-code` and `preflight-report` (JSON
  report path). Failures become workflow annotations; the step summary and
  report are redacted.
- **`config.release-notes` check**: the `release.github.notes_file` and the
  optional `preflight.changelog` must name the tag as a whole version
  (`v1.2.0` is not satisfied by `v1.2.0-rc.1`). Plus `preflight --report
  FILE`.
- **`make publish` gated on the generic preflight**: `make publish-check`
  and `make publish` run `hephaestus preflight --strict` for the tag in
  addition to their own checks.
- **Noarch archives** (`targets: [{os: all, arch: all}]`): one archive for
  every platform, and one formula with a single top-level `url`/`sha256`
  (no `on_macos`/`on_linux` blocks). Mixing a noarch and per-platform
  archive in one formula is an error (select with `ids`).
- **Formula install verification** rejects installs that cannot all
  succeed: a file installed twice, a directory and a file inside it, two
  installs to the same destination. Per-platform exclusive installs are
  allowed.
- **`hephaestus status --tag`**: read-only (`GET` only) state of a release:
  `not-started`, `tagged`, `draft`, `published-incomplete`, `conflict`,
  `published`, `completed`, comparing assets by digest with the local
  manifest and finding each formula's tap pull request; prints the next
  safe action. `--format json`; exit codes `0` read, `1` conflict, `2`
  usage, `3` GitHub could not be read.
- **`hephaestus init --template go|rust|script|prebuilt`**: a starting
  `hephaestus.yaml` (prereleases skip the tap: `skip_upload: auto`). Never
  overwrites a file.
- **Documentation**: [recovery guide](https://github.com/magicdrive/hephaestus/blob/v1.0.0-rc.1/docs/recovery.md)
  (states, what each state allows, 12 failure scenarios and how each is
  verified), [CLI reference](https://github.com/magicdrive/hephaestus/blob/v1.0.0-rc.1/docs/cli.md),
  [compatibility policy](https://github.com/magicdrive/hephaestus/blob/v1.0.0-rc.1/docs/compatibility/policy.md).

## Changed

- **A published release is never edited.** Its title, notes and
  prerelease flag are set while it is a draft and published with it. A
  re-run whose configuration differs (for example, changed notes) logs a
  warning and continues, so the formula and tap can still be resumed. Up to
  v0.2.0, a re-run updated them on a published release, immutable or not.
- **Formula verification** now rejects a formula that installs the same
  file twice; such a formula could never install.

## Safety and testing

- Full-pipeline E2E tests of the CLI against a fake GitHub API: stable,
  prerelease, draft, immutable (complete and missing asset), recovery after
  a failed publish, tap-only re-run, tap pull request failure after
  publishing, and an immutable release resuming the tap. No run leaks a
  token in its output or requests.
- Recovery tests: lost create, upload and publish responses; concurrent
  drafts; digest conflicts on drafts and published releases; tag pointing
  to another commit; immutable re-runs write nothing.
- CI on Linux amd64 and arm64, macOS arm64 and Windows amd64 for every
  change; macOS Intel weekly. Homebrew install tests of generated formulas
  (including noarch) on macOS and Linux.
- The release workflow stops a prerelease whose plan would publish the tap,
  in addition to `skip_upload: auto` and the tap token reaching only the
  tap steps.

## Compatibility

- v0.2.0 configurations load unchanged; Hephaestus's own and
  magicdrive/t's are kept as regression fixtures.
- Configuration, CLI, Action and JSON changes are additions only:
  `preflight.changelog`, noarch targets; `status`, `init`, `preflight
  --report`; Action inputs `strict`, `offline` and the preflight outputs.
- `brews[].skip_upload` still defaults to `false`: a prerelease is
  published to the tap unless `skip_upload: auto` (or `true`) is set. The
  default changes only in a major release.
- From 1.0.0, the stable surface follows the compatibility policy above.

## Limitations

- Windows arm64 archives are built but not tested (no CI runner).
- The Rust template does not cross-compile by itself: each target needs its
  Rust target and a linker, or a build per runner imported as prebuilt
  binaries.
- Recovery is tested against a fake GitHub API; against the real API only
  the normal release path is exercised. The attestation step is covered by
  real releases, not by a test.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v0.2.0...v1.0.0-rc.1
