# Releasing Hephaestus

Hephaestus releases itself with Hephaestus, from `hephaestus.yaml` at the
repository root. **The only thing that publishes is
`.github/workflows/release.yml`, started by pushing a `v*` tag.**
`make publish vX.Y.Z` is the safe way to push that tag: it checks that the
version can be released, asks, creates and pushes an annotated tag, then
follows the workflow run and verifies what it published. It never publishes,
uploads or writes to GitHub itself.

## One-time setup

| Item | Value |
| --- | --- |
| Ruleset on `main` | Active; requires the `ci-gate` status check; forbids deletion and force pushes |
| Ruleset on tags `refs/tags/v*` | Active; forbids **deletion** and **update** (not creation) |
| Environment `release` | Deployment restricted to tags `v*` only (no branch policy); optionally required reviewers |
| Secret in `release` | `HOMEBREW_TAP_GITHUB_TOKEN`: a GitHub App installation token or fine-grained personal access token with **contents: write** and **pull-requests: write** on `magicdrive/homebrew-tap` only. Needed for stable releases; prereleases never receive it |
| Release token | The workflow's `GITHUB_TOKEN` (`contents: write` is granted to the release job only) |
| Repository settings | Immutable releases may be enabled: Hephaestus publishes only complete, verified drafts |
| Locally | `git`, an authenticated `gh` (`gh auth status`), Go, GNU Make (3.81 or later) |

`make publish-check` verifies every row it can read (secret names only, never
values).

## Cutting a release

```sh
make publish-check v0.1.0-rc.1    # optional: the preflight alone
make publish v0.1.0-rc.1          # or: make publish VERSION=v0.1.0-rc.1
make publish v1.0.0
```

The version is a release tag: `vMAJOR.MINOR.PATCH[-PRERELEASE]`
(SemVer 2.0.0 with a leading `v`; no build metadata; `1.0.0` without the `v`
is rejected rather than guessed). Exactly one version, given once: a second
version, both a goal and `VERSION=`, another target in the same `make`
command, or a goal that is not a version is an error before anything runs.
`tag=vX.Y.Z` is accepted too, the form the sibling projects use.

| | Prerelease (`v1.3.0-rc.1`) | Stable (`v1.3.0`) |
| --- | --- | --- |
| GitHub release | published, marked **prerelease**, never "latest" | published, becomes "latest" |
| Assets | 6 archives + `hephaestus_checksums.txt`, all attested | same |
| Homebrew tap | **skipped** (`skip_upload: auto`); the tap token is not passed to any step | formula pull request on `magicdrive/homebrew-tap`, which you review and merge |

### What `make publish` does

1. **Preflight** (read-only; the only local change is a fetch of
   `origin/main` without tags):
   - git: on `main`, not detached, clean worktree including untracked files,
     `origin` is `magicdrive/hephaestus` (HTTPS or SSH), `HEAD` equals
     `origin/main` (nothing unpushed), the tag exists neither locally nor on
     `origin`;
   - GitHub: `gh` authenticated, repository default branch `main`, Actions
     enabled, `release.yml` active, **the `ci.yml` push run for exactly this
     commit succeeded** (a green run of another commit does not count), an
     active tag ruleset forbids deleting and updating the tag and nothing
     blocks creating it, the `main` ruleset requires `ci-gate`, environment
     `release` deploys only from tags matching the version, no release or
     draft exists for the tag, `release.github.draft` is false;
   - prerelease: the release will be a prerelease and every formula skips the
     tap; the tap's head commit is recorded;
   - stable: the tap token secret exists in `release` (by name), the formula
     is published by pull request, the tap repository is reachable, the
     formula goes where Homebrew reads formulae (the root of
     `magicdrive/homebrew-tap`), no pull request or branch for this version
     exists yet, and the download URLs are this release's. Whether the token
     itself can write to the tap is checked by the workflow's read-only tap
     rehearsal, before the release is published.
2. **Confirmation**: repository, branch, full commit SHA, version, release
   type, assets and what happens to the tap. The default is **No**.
   `make publish vX.Y.Z YES=1` confirms non-interactively (only `YES=1` on
   the command line counts, not an environment variable).
3. **Tag**: an annotated tag at the checked commit. Right before the push,
   `origin/main` and the remote tag are checked again (they may have changed
   while the prompt was open). Then exactly that one tag is pushed, never
   forced. If anything fails before the push, the tag exists only locally and
   nothing is published; `publish` does not delete it for you.
4. **Follow**: the `release.yml` run is identified by workflow file, `push`
   event, the tag as head branch, the tagged commit and a creation time after
   the push — never "the latest run". Queued, running, cancelled, timed-out
   and failed runs are reported as such.
5. **Verify**: the release is published (not a draft), its prerelease flag
   and "latest" status are right, it holds exactly the 7 expected assets, each
   downloaded asset matches GitHub's digest and the checksum file, every asset
   has a provenance attestation from `release.yml` for this tag and commit on
   a GitHub-hosted runner, and the tap is either untouched (prerelease: same
   head commit, no pull request, the tap steps skipped) or has a formula pull
   request that changes only `hephaestus.rb`, with this release's URLs and
   checksums (stable).

Exit status: 0 released and verified; 1 failed (or refused before tagging);
2 usage error; 3 monitoring stopped and the outcome is unknown.

### After the tag is pushed

**A pushed tag is final.** It starts the release workflow, which publishes on
its own; nothing on your machine can call it back, and the tag ruleset keeps
the tag from being moved or deleted. Interrupting `make publish` (Ctrl-C, a
lost network, a timeout) stops the monitoring only:

```sh
make publish-status v1.2.3   # tag, run, release, assets, attestations, tap (read-only)
make publish-watch v1.2.3    # find and follow the run again, then verify
```

Running `make publish` again for a version whose tag exists is refused by
the preflight; use `publish-status` and `publish-watch` instead. Two
`make publish` processes in one clone are prevented by a lock file in the git
directory (`hephaestus-publish.lock`); `make -j` runs the tool once.

### Homebrew tap pull request

For a stable version the workflow opens a pull request on
`magicdrive/homebrew-tap` from `hephaestus-X.Y.Z`. A green workflow means the
pull request exists and was verified, **not** that it is merged: review it
(the tap's own checks, `brew audit`) and merge it yourself. `make publish`
never merges it. Users get the new version once it is merged.

## What the release workflow does

`.github/workflows/release.yml`, on a `v*` tag push:

1. **ci** — the complete CI (`ci.yml`) at the tagged commit; nothing is
   published unless `ci-gate` passes.
2. **build** (`contents: read` only — no token that can write and no OIDC
   token):
   - `go run ./internal/cmd/publish plan` — rejects a tag that is not a
     release version before anything is built;
   - `make build` — build Hephaestus from the tagged source;
   - `hephaestus run --stages build,archive,checksum --fresh` — all six
     targets, archives and `hephaestus_checksums.txt`;
   - `scripts/verify-dist.sh dist` — every archive's contents and modes,
     the checksums, and the host binary runs;
   - upload `dist/` as the workflow artifact `dist`.
3. **release** (environment `release`; runs no project build command):
   - `make build` — build Hephaestus itself to publish with;
   - download `dist/` and run `scripts/verify-dist.sh dist` again;
   - **Plan the release** — `internal/cmd/publish plan` decides, with the same
     Go code as the tap stage, whether this is a prerelease and whether the
     tap is published (`tap=publish|skip`). The tag is not pattern-matched in
     YAML;
   - when `tap=publish`: `hephaestus run --stages formula,tap --dry-run` with
     the tap token — read-only checks of the token, the tap's formula
     directory and the pull request branch, before the release becomes
     public;
   - `actions/attest-build-provenance` — provenance for the archives and
     the checksum file (recorded in the public Sigstore transparency log);
   - `hephaestus run --stages release` with `GITHUB_TOKEN` — draft,
     upload, verify, publish, with GitHub-generated notes
     (`generate_notes: true`);
   - when `tap=publish`: `hephaestus run --stages formula,tap` with
     `HOMEBREW_TAP_GITHUB_TOKEN` — the formula and a pull request on
     `magicdrive/homebrew-tap`;
   - when `tap=skip` (prereleases): the same stages **without** the tap
     token; the tap stage only logs why it skips. The release is already
     published at this point, so this step cannot turn the run red.

## Recovering from a failure

| Where it stopped | What exists | What to do |
| --- | --- | --- |
| Preflight or confirmation | nothing | fix the failed check, run `make publish` again |
| Tag creation or the re-check before the push | a local tag only | `git show vX.Y.Z`, then `git tag -d vX.Y.Z` and start again |
| Push | a local tag; maybe the remote one | `make publish-status`: if the tag is on `origin`, `make publish-watch`; otherwise as above |
| Run not found / monitoring stopped | the tag on `origin` | `make publish-watch vX.Y.Z` (or the Actions page); nothing is wrong with the release yet |
| Run failed before the release step (ci, build, verify, attest) | the tag; no release | transient: `gh run rerun <id> --failed`; code fix: merge it and release a **new version** (e.g. the next `-rc.N`) — never move or delete the tag |
| Run failed during the release step | a **draft** release | re-run the failed jobs: the draft is resumed, identical assets are skipped |
| Run failed after publishing | a published release | do not delete it or replace assets; fix forward with a new version |
| Verification failed after a green run | a published release | investigate the failed check (`make publish-status`); never "fix" a published release in place |

## Troubleshooting

- **`ci on commit` fails with "no ci.yml push run"**: the commit was not
  pushed to `main`, or CI has not started. Release only merged, green commits.
- **`in sync with origin` fails**: pull (`git pull --ff-only`) or push your
  commits through a pull request first.
- **`tag ruleset` fails**: the `v*` tag ruleset is disabled or missing
  `deletion`/`update`; a ruleset with a `creation` rule would block the push.
- **`environment` fails**: `release` must deploy only from `v*` tags.
- **`confirmation needs a terminal`**: run in a terminal, or pass `YES=1`.
- **`another publish is running`**: remove `.git/hephaestus-publish.lock`
  if no publish is running.
- **Run stays queued**: an environment approval (required reviewers) or
  runner availability; approve it on the run page, `make publish-watch` keeps
  following it.

## Verifying a release by hand

```sh
gh release download v1.2.3 --repo magicdrive/hephaestus --dir rel && cd rel
sha256sum -c hephaestus_checksums.txt        # shasum -a 256 -c on macOS
for f in hephaestus_*; do
  gh attestation verify "$f" --repo magicdrive/hephaestus \
    --signer-workflow magicdrive/hephaestus/.github/workflows/release.yml \
    --source-ref refs/tags/v1.2.3 --source-digest "$(git rev-parse 'v1.2.3^{commit}')" \
    --deny-self-hosted-runners
done
```

This is what `make publish-status v1.2.3` runs for you. Archive packaging is
deterministic, so rebuilding the tag from a clean checkout with the same Go
toolchain (`make build && ./build/hephaestus run --tag v1.2.3 --stages
build,archive,checksum --fresh`) is expected to reproduce the published
checksums; any difference comes from the compiled binaries.

A prerelease does not update the tap, so `brew install magicdrive/tap/hephaestus`
does not install it; install a prerelease from its archive instead. Formula
installation itself is covered on every change by CI's `homebrew-e2e` job
(`brew audit --strict`, `install`, `test`). Never edit the production tap to
try a prerelease.

## Rehearsing a release

`.github/workflows/release-rehearsal.yml` (run manually, from the default
branch) runs the release path against the real GitHub API without
publishing: the same CI gate and read-only build job, then a publish job in
the `release-rehearsal` environment with `hephaestus.rehearsal.yaml`. That
file equals `hephaestus.yaml` (enforced by `repo_test.go`) except that the
release stays a **draft** (GitHub creates no tag for a draft, and nothing
is public) and the formula pull request goes to
`magicdrive/homebrew-tap-sandbox`. Each publishing stage runs twice to show
that a re-run is a no-op. Provenance attestation is not rehearsed, because
it writes a public transparency log entry.

| Item | Value |
| --- | --- |
| Environment | `release-rehearsal`, deployment restricted to the default branch |
| Secret in that environment | `HOMEBREW_TAP_GITHUB_TOKEN`: fine-grained token with contents and pull-requests write on `magicdrive/homebrew-tap-sandbox` only |
| Sandbox tap | `magicdrive/homebrew-tap-sandbox`, formulae at the root like the production tap |

```sh
gh workflow run release-rehearsal.yml -f version=0.1.0-rehearsal.1
```

The run summary prints the cleanup commands (delete the draft release,
close the sandbox pull request and delete its branch). Use a new rehearsal
version each time, or delete the sandbox branch first.

## Snapshot locally

```sh
make snapshot     # build, archive, checksum and verify into dist/
```
