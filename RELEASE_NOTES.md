# Hephaestus v1.0.0-rc.2

The second release candidate of Hephaestus 1.0. It fixes what dogfooding
v1.0.0-rc.1 found (snapshots of `init` configurations, the `init` formula
test, a false failure in `make publish`'s verification) and hardens
concurrent and repeated releases: Hephaestus no longer deletes a release
in any situation. A prerelease: it is not marked latest and does not update
the Homebrew tap; v0.2.0 stays the latest stable release.

For everything new in 1.0 (preflight in the Action, `status`, `init`,
noarch archives, the recovery guide, the compatibility policy), see the
[v1.0.0-rc.1 notes](https://github.com/magicdrive/hephaestus/releases/tag/v1.0.0-rc.1)
and the [changelog](https://github.com/magicdrive/hephaestus/blob/v1.0.0-rc.2/CHANGELOG.md).

## Fixed since rc.1

- **Snapshots of `init` configurations**: `run --snapshot` skips the formula
  stage as well as release and tap (all three need a tag). With `brews`
  configured, which every `init` template has, rc.1's `run --snapshot`
  failed with "a tag is required". Naming one of them in `--stages` with
  `--snapshot` is an error.
- **`init` formula test**: the templates' test is
  `system bin/"NAME", "--version"`; rc.1's `"#{bin}/NAME"` failed
  `brew audit --strict`.
- **`make publish` verification**: right after the release run, GitHub can
  report the job's steps only partially. The tap-step check now reads them
  again (6 reads, 20 seconds apart) and, if they are still incomplete,
  reports the check as inconclusive (exit 3, re-run `make publish-watch`)
  instead of failing a correct release. A step with the wrong result is
  still a failure. rc.1's `make publish` exited 1 for this reason; the
  release itself was correct.

## Changed: release safety

- **No release is ever deleted.** When two runs race for one tag, the run
  whose new draft loses (to another run's lower-id draft, or to a published
  release) now stops with a conflict and leaves its empty draft for you to
  delete. rc.1 deleted that draft, but once a draft is listed another run
  may already be using it. The GitHub client has no call to delete a
  release.
- **The draft is re-read right before it is published.** A release
  published meanwhile is not edited; a draft whose assets changed (added,
  removed, incomplete or different) or that lost to another release is not
  published. A draft is also re-read before one of its assets is deleted
  and before `draft: true` updates its title or notes, and after publishing
  each asset's digest is checked before its URL is recorded in the
  manifest.
- **A published release's assets are never replaced or deleted**, on
  mutable releases too. `replace_existing_assets` now applies to drafts
  only; an incomplete asset on a published release is reported, not
  removed. (rc.1 and v0.2.0 replaced a differing asset of a published
  mutable release when `replace_existing_assets: true` was set.)
- **An incomplete asset seen right after a failed upload** is deleted only
  when it is older than the 10-minute grace period: a recent one cannot be
  told apart from another run's upload in progress, so the asset fails and
  a later re-run continues.

## Testing

- Regression tests for each fix: snapshots of every `init` template (Go,
  script and prebuilt in CI; Rust when cargo and the template's four
  targets are installed), the formula test line, and lagging step data
  (complete later; never complete is inconclusive).
- Recovery and concurrency tests against a fake GitHub API: the losing
  draft (to a lower-id draft and to a published release) deletes nothing
  and a re-run continues with the winner; a lost create reusing another
  run's draft; the pre-publish re-read (published meanwhile, asset added,
  changed or removed, another release published); a draft published
  before an asset delete or a `draft: true` update; another build's
  content published meanwhile; starters after a failed
  upload; published assets never replaced or deleted. Plus the rc.1 suite:
  lost create, upload and publish responses, digest conflicts, tag pointing
  to another commit, immutable re-runs, tap pull request failure after
  publishing.

## Compatibility

- v0.2.0 and rc.1 configurations load unchanged. No configuration key,
  CLI flag, Action input or output, or check ID was removed or renamed.
- Two behaviors are narrower, both for safety: `replace_existing_assets`
  no longer touches a published release, and a lost concurrent race stops
  with a conflict instead of deleting a draft.
- `brews[].skip_upload` still defaults to `false`; `init` templates set
  `skip_upload: auto`.

## Limitations

- Windows arm64 archives are built but not run (no CI runner).
- The Rust template does not cross-compile by itself: each target needs its
  Rust target and a linker, or a build per runner imported as prebuilt
  binaries.
- Recovery is tested against a fake GitHub API; against the real API only
  the normal release path is exercised. The attestation step is covered by
  real releases, not by a test.
- GitHub has no conditional update: between a re-read and the write that
  follows it (one API call), another writer could still change the
  release.
- A transient upload failure that leaves an incomplete asset now fails the
  run instead of retrying at once; re-run after 10 minutes.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.0.0-rc.1...v1.0.0-rc.2
