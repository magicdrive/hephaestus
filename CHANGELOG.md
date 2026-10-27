# Changelog

All notable changes to Hephaestus. The notes of each release are published
from `RELEASE_NOTES.md` (`release.github.notes_file`).

## Unreleased

- **Release workflow checks**: `hephaestus preflight` reads the release
  workflow (`preflight.workflow`, default `.github/workflows/release.yml`)
  and the CI workflow it runs from the work tree, and checks the
  conventions `hephaestus publish` relies on, which were verified only after
  the release was published: `workflow.release` (tag pushes only, read-only
  default permissions, one publishing job in an environment that needs the
  CI job), `workflow.pins` (every action pinned to a commit SHA, Hephaestus
  pins with their release as a comment and CI on the release's commit),
  `workflow.tap` (the tap token only in the two named tap steps, after the
  plan step, with `if: steps.plan.outputs.tap == 'publish'`),
  `workflow.attestation` (every planned asset attested before publishing)
  and `workflow.ci-gate` (`ci-gate` needs every CI job, `if: always()`).
  They read YAML only, so they work for any language, and fail closed on
  what they cannot read unambiguously. They are **warnings**: a violation
  makes the preflight `WARN` (exit 0) and fails `--strict`, and so
  `hephaestus publish` now stops before pushing the tag instead of after
  publishing; `preflight.skip` turns one off. `hephaestus check
  --workflows` runs the same checks without a tag (for pull request CI) and
  exits 1 on a violation.
- The tap stage (pull request mode) reads the other open pull requests
  against the tap's base branch before proposing a formula. When one
  proposes a newer version (by SemVer precedence), nothing is written and
  the formula is skipped, as for a newer base branch: merging the older
  pull request last would have downgraded the formula. Older ones are named
  in a warning and in the new pull request's body, which now states the
  merge order; the scan is repeated after opening, so racing releases
  report each other. `brews[].allow_downgrade` turns the skip into a
  warning. New `cmd/tap-version-guard` and
  `examples/workflows/tap-version-guard.yml` let a tap's CI fail a change
  that lowers a formula's version (docs/tap-version-guard.md).
- The publish lock is an operating system file lock (flock on Unix,
  LockFileEx on Windows) instead of a file created with `O_EXCL`. A publish
  killed with `SIGKILL` (or crashed) left `hephaestus-publish.lock` behind
  and every later publish failed until the file was removed by hand; the
  system now releases the lock with the process, a leftover file (also one
  from an older version) is locked again, and a live publish's lock is
  still refused, naming its pid, host and start time. Publish refuses to
  run on a file system that cannot lock.
- The tap stage verifies a pull request head branch that someone else
  created between its check that the branch is missing and its create (a
  422 "Reference already exists"), as it verifies any existing branch: it
  was used as it was, so the pull request could propose that branch's other
  changes under the formula update's title. A create whose response was
  lost (5xx, timeout) is followed by a read of the branch, so a branch that
  was created is used (after the same verification when it is not at the
  commit sent) instead of failing the tap step.
- The release stage stops before it creates, uploads or publishes anything
  when the commit being released is known (`--commit`, `$GITHUB_SHA`) but
  the tag cannot be read (an API error other than 404: missing permission,
  an outage that outlasts the retries). It only warned and went on, and since
  GitHub ignores `target_commitish` for an existing tag, the build could have
  been published under a tag of another commit.
- `scripts/fuzz.sh` fails when `go list` or `go test -list` fails, instead of
  fuzzing nothing and passing; `scripts/go-env-snapshot.sh` fails when `go
  version` or `go env` fails instead of printing an empty value, and runs
  both outside any module: from the repository root, `go.mod`'s toolchain
  line made a runner's older Go download that toolchain, so CI's
  action-isolation, action-install and action-release jobs depended on
  the Go download servers.
- Tests: every check `hephaestus publish` makes between creating the tag and
  pushing it (the re-fetch, the release branch's head on GitHub, the tag on
  origin) stops the publish when it fails or cannot be made, and `make
  publish*` exits non-zero whenever the publish tool does.
- `hephaestus publish` says to delete the local tag when another clone or
  machine pushed the same tag at another commit first; tests pin that such a
  race never replaces origin's tag, and that a tag pushed elsewhere at the
  checked commit is followed as this release.

## v1.3.0

`hephaestus publish` tells the repository's default branch from the release
branch (`-branch`, `preflight.branch`, else the default branch) and checks
everything that guards a release on the release branch. The full notes are
in [RELEASE_NOTES.md](RELEASE_NOTES.md); the changes are listed under
v1.3.0-rc.1 below.

- Since v1.3.0-rc.1: `action/VERSION` names v1.3.0. v1.3.0-rc.1 was released
  and verified with this `make publish`, and its binaries ran on all six
  platforms (`verify-release.yml`).

## v1.3.0-rc.1

First release candidate of 1.3; full notes in
[RELEASE_NOTES.md](RELEASE_NOTES.md).

- `hephaestus publish` separates the release repository's **default branch**
  on GitHub from the **release branch** releases are cut from. The release
  branch is `-branch <name>`, else `preflight.branch` of `hephaestus.yaml`
  (which `hephaestus preflight` already read), else the default branch
  (`main`, `master`, `develop` or any name) instead of only `main`: a
  repository whose default branch is `develop` can release from
  `release/stable`. The preflight reports both (`default branch`, `release
  branch`) and checks the release branch everywhere: it must exist on GitHub
  and be a branch name (not a tag, a SHA, a full ref or a refspec); the clone
  must be on it with `HEAD` = `origin/<branch>` = its head on GitHub; CI must
  have succeeded on it for the commit; a ruleset on it must require
  `ci-gate`; and its head on GitHub is checked again right before the tag
  push. A local `refs/remotes/origin/HEAD` naming another branch than the
  default branch fails the preflight. When a branch cannot be read, nothing
  further is checked and nothing is tagged. The ruleset check is named after
  the branch (`main ruleset` for `main`). `plan` and `status` are unchanged.
- A ruleset that covers `~DEFAULT_BRANCH` now counts only for the
  repository's default branch, in `hephaestus publish` and in `hephaestus
  preflight` (`github.ruleset.branch`, the required checks of `github.ci`):
  it no longer counts as protecting a configured release branch with another
  name.
- CI: the Homebrew end-to-end tests trust the throw-away tap they test
  (`brew trust`, Homebrew 7.0.11), and only that tap.

## v1.2.0

The GitHub Action runs the verified release binary instead of compiling
Hephaestus, `hephaestus publish` releases any project, and the fixes prepared
for v1.1.1 (never released) are included. The full notes are in
[RELEASE_NOTES.md](RELEASE_NOTES.md); the changes are listed under
v1.2.0-rc.1 below.

- Since v1.2.0-rc.1: `action/VERSION` names v1.2.0. CI runs the action of
  v1.2.0-rc.1 through `uses:` on six platforms (`action-release`): the
  release path, outputs and an unchanged toolchain.

## v1.2.0-rc.1

First release candidate of 1.2; full notes in
[RELEASE_NOTES.md](RELEASE_NOTES.md). It includes the fixes prepared for
v1.1.1, which was never released.

### Changed

- **The GitHub Action runs the verified release binary instead of compiling
  Hephaestus.** It reads the release from `action/VERSION`, then:
  - downloads that release's archive for the runner, and its checksum file,
    from `github.com/magicdrive/hephaestus/releases/download/<tag>/` only;
  - checks the archive's SHA-256;
  - verifies its provenance attestation: signed by `release.yml` on a
    GitHub-hosted runner for that tag and, when pinned by SHA, for exactly
    the pinned commit;
  - extracts only the regular `hephaestus[.exe]` at the archive root, and
    checks the version the binary reports.

  Any failure stops the action, with no fallback. No Go toolchain is
  downloaded or set up, and nothing is written to `$GITHUB_PATH` or
  `$GITHUB_ENV`. Linux, macOS and Windows on x64 and ARM64 are supported, as
  before. The action must be referenced by a release tag or the commit that
  tag points to; a branch reference fails.
- The release workflow refuses to build a tag that `action/VERSION` does not
  name.

### Added

- `hephaestus publish`: the release tool Hephaestus uses for itself, for any
  project with the same release workflow conventions
  ([docs/publish.md](docs/publish.md)). `make publish vX.Y.Z` runs it in
  order:
  1. preflight;
  2. confirmation;
  3. annotated tag;
  4. follow the release workflow;
  5. verify assets, checksums, attestations and the tap pull request.
- Action `command: plan`, with the outputs `prerelease` and `tap`: the
  release plan of a tag, so a release job can skip, and never give the token
  to, the tap steps of a prerelease. It refuses a prerelease that would
  update the tap.

### Fixed

- **The Action no longer changes the caller's Go toolchain.** v1.1.0 ran
  `actions/setup-go`, which put Go 1.26.0 first on the job's `PATH` and
  exported `GOTOOLCHAIN=local`. A module requiring a newer Go failed, and
  others were built with an older toolchain.
- **Redaction no longer hides paths under the current directory.** The shell's
  `PWD` and `OLDPWD` are not credentials. A value matched only by its
  variable's name is not redacted when it is the path of an existing file or
  directory. Known tokens are always redacted.
- **`--fresh` deletes the previous run's archives, checksum files and
  formulae from `dist`** when the run writes them again. Only unchanged files
  that Hephaestus wrote itself are deleted.

### Compatibility

- No configuration key, flag, Action input or output, JSON field or check ID
  was removed or renamed. `publish` is a new command.
- **The Action now requires a release reference:** a release tag, or the full
  SHA of the commit that tag points to. `@main` and other branches fail.
- **The Action no longer provides Go:** a project that relied on the
  Action's Go must set up its own. The examples always did.
- `{{ .Env.PWD }}` and `{{ .Env.OLDPWD }}` are now visible to templates, and
  build commands now receive `PWD` and `OLDPWD`.

## v1.1.0

Shell completion; full notes in [RELEASE_NOTES.md](RELEASE_NOTES.md). A
minor release: it adds a command and a configuration key
([compatibility policy](docs/compatibility/policy.md)).

### Added

- `hephaestus completion bash|zsh|fish` prints a completion script:
  commands, each command's flags, and the fixed values of `--format`,
  `--template`, `--phase`, `--stages` and `--skip` (also in `--flag=value`
  form), file names for `--config`, `--report` and `--output`. Candidates
  come from the same flag definitions the commands parse (the hidden
  `__complete` command), which read no configuration, token or network.
- `brews[].completions` installs bash, zsh and fish completions with a
  formula, for a CLI in any language: `generate` (Homebrew's
  `generate_completions_from_executable`, with `args`,
  `shell_parameter_format` and `shells`) and/or `files` shipped in the
  archive (`<shell>_completion.install`). Validated: shells, the format,
  the executable installed into `bin` for every archive, no shell installed
  twice, shipped files present in each archive.
- Hephaestus's own formula installs its three completion scripts, and
  `make publish` verifies that the tap pull request's formula contains the
  configured completion install lines.
- CI: completion in real bash (including macOS's 3.2), zsh and fish on
  Linux and macOS; a Homebrew test of a shell-script fixture and of
  Hephaestus itself, upgraded from the 1.0.0 formula.

### Changed

- `hephaestus help` lists `completion` and `help`.

### Compatibility

- Additions only: no command, flag, configuration key, Action input or
  output, JSON field or check ID changed. Formulae without
  `completions` are generated byte for byte as before.
- A configuration that uses `brews[].completions` is rejected by
  Hephaestus 1.0.0 and older (unknown keys are errors).

## v1.0.0

First stable release of 1.0; full notes in
[RELEASE_NOTES.md](RELEASE_NOTES.md). This section summarizes the changes
since v0.2.0; the release candidates below record each step. From 1.0.0,
Hephaestus follows the [compatibility policy](docs/compatibility/policy.md).

### Added

- **Preflight**, extended: `preflight` in the GitHub Action (inputs
  `strict` and `offline`; outputs `preflight-status`,
  `preflight-exit-code` and `preflight-report`; annotations, redacted
  summary). The check `config.release-notes` requires the notes file and
  the optional `preflight.changelog` to name the tag. Plus `preflight
  --report FILE`, and `make publish` gated on `preflight --strict`.
- **Noarch archives** (`targets: [{os: all, arch: all}]`): one archive and
  one formula `url` for every platform.
- **Formula quality**: formula generation rejects installs that cannot all
  succeed (a file installed twice, a directory and a file inside it, two
  installs to one destination).
- **`hephaestus status --tag`**: the read-only state of a release and the
  next safe action ([docs/recovery.md](docs/recovery.md)).
- **`hephaestus init --template go|rust|script|prebuilt`**.
- **Verify release workflow** (`.github/workflows/verify-release.yml`,
  run manually): runs a published release's binaries on Linux, macOS and
  Windows, amd64 and arm64.
- [CLI reference](docs/cli.md), [compatibility policy](docs/compatibility/policy.md),
  [recovery guide](docs/recovery.md).

### Changed

- **Immutable release protection**: a published release's title, notes
  and prerelease flag are never edited (v0.2.0 updated them on a re-run). A
  published release's assets are never replaced or deleted:
  `replace_existing_assets` applies to drafts only.
- **Concurrent release safety**: Hephaestus never deletes a release. A run
  that loses a race for a tag stops with a conflict instead of deleting its
  draft. A draft is re-read before it is published, before one of its
  assets is deleted, and before `draft: true` updates it. Each asset's
  digest is checked before its URL is recorded.
- **Recovery hardening**: an incomplete asset seen after a failed upload is
  deleted only when it is older than the 10-minute grace period.
- The release workflow stops a prerelease whose plan would publish the
  tap.

### Fixed

- **Snapshots**: `run --snapshot` failed with "a tag is required" for any
  configuration with `brews`. It now skips the formula stage, as it skips
  release and tap.
- **`init` templates**: the formula test is `system bin/"NAME",
  "--version"`, which passes `brew audit --strict` (fixed in rc.2).
- **Publish verification**: `make publish` re-reads step data that GitHub
  reports only partially right after a run. If the data stays incomplete,
  the result is inconclusive (exit 3) rather than a failure (fixed in rc.2).

### Security

- Publishing tokens stay out of the build's environment and are redacted
  from output, reports and step summaries, including the preflight report
  and annotations.
- Releases are created only from complete, verified drafts, and every
  archive and the checksum file carry a build provenance attestation. The
  `verify-release` workflow has read-only permissions.

### Compatibility

- v0.2.0 configurations load unchanged (`testdata/compat/v0.2.0`).
  Configuration keys, CLI commands and flags, Action inputs and outputs,
  JSON fields and check IDs were only added; nothing was removed or
  renamed.
- Narrower behavior, for safety: a published release is not edited, and
  its assets are not replaced or deleted; `replace_existing_assets` applies
  to drafts only; a draft that lost a race is not deleted. Also: `run
  --snapshot` skips the formula stage, and a formula configuration that
  installs the same file twice is rejected.
- `brews[].skip_upload` still defaults to `false`; set `auto` to keep
  prereleases out of the tap.

### Known Limitations

- The Rust template needs each target's Rust target and linker; it was
  verified for the host target only.
- Recovery failure injection is tested against a fake GitHub API.
- GitHub has no conditional update: between a re-read and the write that
  follows it, another writer can still change the release.
- An incomplete asset left by a failed upload waits 10 minutes before a
  re-run removes it.

## v1.0.0-rc.2

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
  published. A draft is also re-read before one of its assets is deleted
  and before `draft: true` updates its title or notes; after publishing,
  each asset's digest is checked before its URL is recorded.
- A published release's assets are never replaced or deleted, on mutable
  releases too: `replace_existing_assets` now applies to drafts only, and
  an incomplete asset on a published release is reported, not removed.
- An incomplete asset seen right after a failed upload is deleted only when
  it is older than the 10-minute grace period: a recent one cannot be told
  apart from another run's upload in progress.

### Compatibility

- No configuration key, CLI flag, Action input or output, or check ID was
  removed or renamed; v0.2.0 and rc.1 configurations load unchanged.
- Narrower for safety: `replace_existing_assets` applies to drafts only,
  and a lost concurrent race stops with a conflict instead of deleting a
  draft.

### Limitations

- Windows arm64 archives are built but not run (no CI runner).
- GitHub has no conditional update: between a re-read and the write that
  follows it, another writer could still change the release.
- A transient upload failure that leaves an incomplete asset fails the run;
  re-run after 10 minutes.

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
