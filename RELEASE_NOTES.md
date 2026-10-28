# Hephaestus v1.4.0

`hephaestus preflight`, and so `hephaestus publish`, now checks the release
workflow's conventions before the tag is pushed; the Homebrew tap stage no
longer lets two release pull requests downgrade a formula through their merge
order; and two new commands guard the tap's merges: `cmd/tap-version-guard`
for the tap's required check and `cmd/pr-gate` for a fail-closed merge. The
publish lock no longer outlives a killed process.

## Release workflow checks

`hephaestus preflight` reads the release workflow (`preflight.workflow`,
default `.github/workflows/release.yml`) and the CI workflow it runs from the
work tree, and checks the conventions `hephaestus publish` relies on, which
were verified only after the release was published:

| Check | What it requires |
| --- | --- |
| `workflow.release` | tag pushes only, read-only default permissions, one publishing job in an environment that needs the CI job |
| `workflow.pins` | every action pinned to a commit SHA; Hephaestus pins carry their release as a comment, and CI runs the release's commit |
| `workflow.tap` | the tap token only in the two named tap steps, after the plan step, with `if: steps.plan.outputs.tap == 'publish'` |
| `workflow.attestation` | every planned asset attested before publishing |
| `workflow.ci-gate` | `ci-gate` needs every CI job, with `if: always()` |

They read YAML only, so they work for any language, and fail closed on what
they cannot read unambiguously. They are **warnings**: a violation makes the
preflight `WARN` (exit 0) and fails `--strict`, so `hephaestus publish` stops
before pushing the tag. `preflight.skip` turns one off.

```sh
hephaestus check --workflows   # the same checks without a tag, for pull request CI (exit 1 on a violation)
```

See [docs/preflight.md](https://github.com/magicdrive/hephaestus/blob/v1.4.0/docs/preflight.md).

## Homebrew tap: formula downgrades

In pull request mode every release opens its own formula pull request; merged
in the wrong order, an older one lowered the formula. The tap stage now:

- reads the other open pull requests against the tap's base branch before
  proposing a formula. When one proposes a newer version (by SemVer
  precedence), nothing is written and the formula is skipped, as for a newer
  base branch. Older ones are named in a warning and in the new pull
  request's body, which states the merge order; the scan is repeated after
  opening, so racing releases report each other. `brews[].allow_downgrade`
  turns the skip into a warning;
- counts only pull requests from a branch of the tap (or of the configured
  head repository). One from someone else's fork is a warning: anyone can
  open one on a public tap;
- **fails, before writing anything and in a dry run too**, when the base
  branch, the pull request branch or an open pull request from the tap
  already has the version being published with another sha256 for the same
  archive url: the release's archives were replaced or tampered with. The
  release workflows' read-only rehearsal stops such a release before it is
  published. `allow_downgrade` does not apply. `tap.ChecksumChanges` is the
  rule, for the tap's CI to reuse.

### `cmd/tap-version-guard`

A check for the tap's own CI that fails a change lowering a formula's
version:

```sh
go run github.com/magicdrive/hephaestus/cmd/tap-version-guard@<commit> [-base HEAD^1 [-each] | -against <tip>] [-head HEAD] [-strict] \
  [-rollback PATH=FROM->TO ... | -approvals <comments.json> -permissions <permissions.json> -approval-head <sha> [-approval-before <time>]] \
  [-summary <file>] [-allow-downgrade]
```

- `-against <tip>` checks an unmerged pull request head against the base
  branch's current tip; `-each` checks every first-parent step of
  `base..head` (a merge queue entry, a push of several merges).
- `-strict`, for a required check, also fails a formula with no or a
  non-semantic version, versions that cannot be compared, the same version
  with other artifacts, a url of another version, a removed formula, a
  formula that is not a regular file, an unsafe path and a `Formula` entry
  that is not a directory. It reads only the formulae Homebrew loads, from
  git objects only.
- A rollback is authorized for exactly one downgrade, `PATH=FROM->TO`: by
  `-rollback`, or by an approval comment (`/formula-rollback` with `head:`,
  `formula:` and `reason:`) whose author is an admin of the tap, that was
  never edited and names the pull request head. Every other finding stays an
  error, and an authorization the change does not use fails.
  `-allow-downgrade` stays for compatibility.

[examples/workflows/tap-version-guard.yml](https://github.com/magicdrive/hephaestus/blob/v1.4.0/examples/workflows/tap-version-guard.yml)
runs the required check on `pull_request_target` (the default branch's
workflow; the pull request is fetched as objects only), needs the
`formula-rollback` label and an admin's approval for a rollback, posts the
commit status `version-guard` as well, and re-checks after each merge,
opening an issue when a downgrade landed. See
[docs/tap-version-guard.md](https://github.com/magicdrive/hephaestus/blob/v1.4.0/docs/tap-version-guard.md).

## Merge gate

`cmd/pr-gate` decides whether a pull request may be merged and, only with
`-merge`, merges it pinned to the reviewed head (`PUT pulls/{n}/merge` with
`sha`, sent once) and verifies the merge commit's parents. Every condition
fails closed: open, not a draft, exact head and base commits, behind 0,
mergeable and clean, required checks read from the branch rules and branch
protection (or named with `-require`), every check, status and workflow run
completed, and a required check satisfied only by `success`. Without
`-merge` it only reads. See
[docs/pr-gate.md](https://github.com/magicdrive/hephaestus/blob/v1.4.0/docs/pr-gate.md).

## Fixes

- **Publish lock.** The lock on `hephaestus-publish.lock` is an operating
  system file lock (flock on Unix, LockFileEx on Windows) instead of a file
  created with `O_EXCL`. A publish killed with `SIGKILL` (or crashed) left the
  file behind and every later publish failed until it was removed by hand;
  the system now releases the lock with the process. A live publish's lock is
  still refused, naming its pid, host and start time. Older versions, which
  lock with `O_EXCL` on the same file, are still excluded. Publish refuses a
  file system that cannot lock, and a clone on a network or user-space file
  system (NFS, SMB, WebDAV, FUSE, 9p, a network drive), and checks right
  before creating and pushing the tag that the lock file is still the one it
  locked.
- **Tap head branch.** A pull request head branch that someone else created
  between the tap stage's check and its create (a 422 "Reference already
  exists") is verified like any existing branch, instead of being used as it
  was. A create whose response was lost (5xx, timeout) is followed by a read
  of the branch, instead of failing the tap step.
- **Release stage.** When the commit being released is known (`--commit`,
  `$GITHUB_SHA`) but the tag cannot be read (an API error other than 404),
  the release stage stops before it creates, uploads or publishes anything.
  It only warned and went on, so the build could have been published under a
  tag of another commit.
- **`hephaestus publish`** says to delete the local tag when another clone
  pushed the same tag at another commit first.
- **Scripts.** `scripts/fuzz.sh` fails when `go list` or `go test -list`
  fails, instead of fuzzing nothing; `scripts/go-env-snapshot.sh` fails when
  `go version` or `go env` fails, and runs them outside any module. `make
  fmt-check` fails when `gofmt` itself fails.
- **Hephaestus's own release workflow.** A new `provenance` job releases only
  a tag whose commit is in the history of `main`, and the release jobs run on
  `ubuntu-24.04` instead of `ubuntu-latest`.

## Upgrade notes

These changes fail closed where the previous version went on:

- A release workflow that violates a `workflow.*` convention now stops
  `hephaestus publish` (and `hephaestus preflight --strict`) before the tag
  push. Fix the workflow, or turn the check off with `preflight.skip`.
- A tap that already has the version being published with another sha256
  for the same archive url now fails the tap stage, in a dry run too. Release
  a new version instead.
- A `hephaestus-publish.lock` written by an older version (`pid N`) is
  refused, never taken over by its pid: empty the file once no older publish
  runs.
- `hephaestus publish` refuses a clone on a network or user-space file
  system; publish from a clone on a local disk.

## Compatibility

- **Unchanged:** configuration keys, the Action, `publish plan` and `publish
  status`, and the existing JSON fields and check IDs.
- **New:** the preflight checks `workflow.release`, `workflow.pins`,
  `workflow.tap`, `workflow.attestation` and `workflow.ci-gate` (warnings),
  `hephaestus check --workflows`, `cmd/tap-version-guard`, `cmd/pr-gate` and
  `examples/workflows/tap-version-guard.yml`.
- **Changed:** in pull request mode the tap stage skips a formula when an
  open pull request from the tap proposes a newer version (a warning with
  `brews[].allow_downgrade`), and fails on the same version with other
  checksums (above).

## Known limitations

- `cmd/pr-gate` serializes merges on one machine with a lock file created
  with `O_EXCL` in the temporary directory: a gate killed with `SIGKILL`
  leaves it behind (remove it by hand), and another user of a shared
  temporary directory can create it first. A merge's safety rests on the
  pinned `sha` and the check of the merge commit's parents, not on the lock.
- `hephaestus check` shows the tap pull request branch as a template;
  `hephaestus preflight --tag` renders it.

## Verification

CI succeeded on the release commit's parent on `main` (run 40511957192),
including the tests added with these changes: every check between creating
and pushing the tag, failure injection for every GitHub write of a release,
the race of two release pull requests (`TestTOCTOU`), and `go test` without
network.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.3.0...v1.4.0
