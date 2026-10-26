# Publishing a release: `hephaestus publish`

`hephaestus publish vX.Y.Z` is the safe way to push the release tag of a
project that releases with Hephaestus. It checks that the version can be
released, asks, creates and pushes one annotated tag, then follows the
release workflow run that the push starts and verifies what it published.
**It never publishes, uploads or writes to GitHub itself**: the project's
`.github/workflows/release.yml`, started by the tag push, stays the only
publisher. Hephaestus releases itself the same way (`make publish vX.Y.Z`,
[releasing.md](releasing.md)).

Most projects wrap it in `make publish vX.Y.Z`, as below. The project must
follow the [conventions](#conventions): publish checks them and refuses to
tag when one is not met; they are not settings.

## Commands

```text
hephaestus publish [command] [flags] vX.Y.Z
```

| Command | Make target | Writes | Purpose |
| --- | --- | --- | --- |
| *(none)* | `make publish vX.Y.Z` | the tag (local and pushed), a fetch of `origin/<release branch>` | preflight, confirmation, annotated tag, tag push, follow the release run, verify the release |
| `check` | `make publish-check vX.Y.Z` | a fetch of `origin/<release branch>` | the preflight only |
| `status` | `make publish-status vX.Y.Z` | nothing | what exists for a version: local and remote tag, release run, release, and (when published) the full verification |
| `watch` | `make publish-watch vX.Y.Z` | nothing | find and follow the release run of an already pushed tag, then verify |
| `plan` | — | nothing | what the release workflow does for a tag, as `key=value` lines for `$GITHUB_OUTPUT` (below) |
| `help` | `make publish-help` | nothing | how publishing works and its safety rules |

`publish` may also be named explicitly (`hephaestus publish publish vX.Y.Z`).
Flags come before the version:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config` | `hephaestus.yaml` | configuration file; run from the repository root |
| `-yes` | off | confirm without the prompt (explicit non-interactive mode) |
| `-from-make` | off | read the version from the environment a Makefile sets (below) instead of the command line |
| `-discover-timeout` | `5m` | how long to wait for the release run to appear |
| `-watch-timeout` | `90m` | how long to wait for the release run to finish |
| `-poll` | `15s` | polling interval |

The version is a release tag: `vMAJOR.MINOR.PATCH[-PRERELEASE]` (SemVer 2.0.0
with a leading `v`, no build metadata). `1.2.3` without the `v` is rejected,
not guessed: the tag cannot be taken back once pushed.

Exit codes: `0` released and verified (`check`: preflight passed; `status`:
nothing failed), `1` failed or refused, `2` usage (including an invalid
version), `3` **outcome unknown**: monitoring stopped (network, Ctrl-C,
timeout, a verification that could not complete) and the release may still be
running or fine.

`plan` prints, for example:

```text
tag=v1.3.0-rc.1
version=1.3.0-rc.1
prerelease=true
tap=skip
```

`prerelease` is the GitHub release's prerelease flag (from
`release.github.prerelease`); `tap` is `publish` when a formula is published
for this version and `skip` when every formula skips the tap
(`brews[].skip_upload`), decided by the same code as the tap stage.

## `make publish vX.Y.Z` in your project

Add this to the project's `Makefile` (GNU Make 3.81 or later). It is the
Makefile Hephaestus uses for itself, with `hephaestus publish` in place of its
in-repository tool:

```make
# Release with Hephaestus (https://github.com/magicdrive/hephaestus/blob/main/docs/publish.md):
#   make publish v1.2.3          (or VERSION=v1.2.3, or tag=v1.2.3)
#   make publish-check v1.2.3    preflight only
#   make publish-status v1.2.3   what exists for a version
#   make publish-watch v1.2.3    follow an already pushed tag
#   make publish-help
# The version given as a goal is a no-op goal (.DEFAULT). It reaches
# hephaestus through the environment, never through a shell command line,
# and hephaestus validates it. YES=1 on the command line skips the prompt.
HEPHAESTUS = hephaestus
PUBLISH_TARGETS = publish publish-check publish-status publish-watch publish-help

ifneq ($(filter $(PUBLISH_TARGETS),$(MAKECMDGOALS)),)
PUBLISH_GOAL := $(firstword $(MAKECMDGOALS))
PUBLISH_ARGS := $(wordlist 2,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS))
ifeq ($(filter $(PUBLISH_TARGETS),$(PUBLISH_GOAL)),)
$(error publish targets come first and alone: make publish v1.2.3)
endif
ifneq ($(filter $(PUBLISH_TARGETS),$(PUBLISH_ARGS)),)
$(error give one publish target at a time)
endif
ifneq ($(word 2,$(PUBLISH_ARGS)),)
$(error one version is expected, got: $(PUBLISH_ARGS))
endif
ifeq ($(PUBLISH_GOAL),publish-help)
ifneq ($(PUBLISH_ARGS),)
$(error publish-help takes no version)
endif
endif
export HEPHAESTUS_PUBLISH_ARGS := $(PUBLISH_ARGS)
export HEPHAESTUS_PUBLISH_VERSION := $(if $(filter command line,$(origin VERSION)),$(VERSION))
export HEPHAESTUS_PUBLISH_TAG := $(if $(filter command line,$(origin tag)),$(tag))
PUBLISH_YES := $(if $(and $(filter command line,$(origin YES)),$(filter 1,$(YES))),-yes)
ifneq ($(PUBLISH_ARGS),)
ifeq ($(filter v0% v1% v2% v3% v4% v5% v6% v7% v8% v9% 0% 1% 2% 3% 4% 5% 6% 7% 8% 9%,$(PUBLISH_ARGS)),)
$(error "$(PUBLISH_ARGS)" is not a version: make $(PUBLISH_GOAL) v1.2.3)
endif
# The version goal has no rule of its own: it is the publish target's
# argument. (No other target may start with v<digit>.)
.DEFAULT:
	@:
endif
endif

.PHONY: publish publish-check publish-status publish-watch publish-help
publish:
	@$(HEPHAESTUS) publish -from-make $(PUBLISH_YES)
publish-check:
	@$(HEPHAESTUS) publish check -from-make
publish-status:
	@$(HEPHAESTUS) publish status -from-make
publish-watch:
	@$(HEPHAESTUS) publish watch -from-make
publish-help:
	@$(HEPHAESTUS) publish help
```

How the version gets in:

| Make command line | Environment `hephaestus publish -from-make` reads |
| --- | --- |
| `make publish v1.2.3` | `HEPHAESTUS_PUBLISH_ARGS=v1.2.3` |
| `make publish VERSION=v1.2.3` | `HEPHAESTUS_PUBLISH_VERSION=v1.2.3` |
| `make publish tag=v1.2.3` | `HEPHAESTUS_PUBLISH_TAG=v1.2.3` |
| `make publish v1.2.3 YES=1` | as above, plus the `-yes` flag |

Exactly one version, given once: a second version, both a goal and
`VERSION=`, or another target in the same `make` command is an error before
anything runs. Only values given on the make command line count (`VERSION`
or `YES` exported in the shell, or a `VERSION` the Makefile defines for its
own use, are ignored). With `-from-make`, `hephaestus publish` takes no
version argument, and the next steps it prints name the make targets
(`make publish-watch v1.2.3`); run directly, they name its own commands
(`hephaestus publish watch v1.2.3`).

`HEPHAESTUS` must be `hephaestus` v1.2.0 or later: an installed binary, or a
pinned `go run github.com/magicdrive/hephaestus@v1.2.0`. `make` reports any
non-zero exit (including `3`, outcome unknown) as its own failure; read the
summary.

## Release branch

The **default branch** is the release repository's default branch on GitHub.
The **release branch** is the branch releases are cut from. Unless one is
named, it is the default branch; when one is named, it need not be:

| Default branch on GitHub | Named release branch | Release branch |
| --- | --- | --- |
| `main` | — | `main` |
| `master` | — | `master` |
| `develop` | — | `develop` |
| `develop` | `release/stable` | `release/stable` |
| `main` | `release/1.x` | `release/1.x` |
| `trunk` | `production` | `production` |

It is named, in this order of precedence, by:

1. `-branch <name>` on the command line (`hephaestus publish -branch release/stable vX.Y.Z`,
   `hephaestus publish check -branch release/stable vX.Y.Z`);
2. `preflight.branch` in `hephaestus.yaml`, which `hephaestus preflight` also
   reads;
3. else the default branch.

```yaml
preflight:
  branch: release/stable
```

The preflight prints both, and where the release branch came from:

```
  [ok  ] default branch               develop (GitHub, origin/HEAD)
  [ok  ] release branch               release/stable (preflight.branch) at 1a2b3c4d5e6f on GitHub
```

Everything that guards the release applies to the release branch, never to
the default branch on its behalf: it is the branch checked out and fetched,
the branch CI must have succeeded on for the tagged commit, the branch whose
ruleset must require `ci-gate`, the branch whose head on GitHub must still be
the tagged commit right before the push, and the branch `hephaestus preflight
--strict` checks. The release branch must be a branch name that exists on
GitHub: a tag, a commit SHA, a full ref (`refs/heads/...`) or anything that
changes a refspec (`:`, `*`, a leading `+`) fails the preflight before anything
is fetched or tagged. `plan` and `status` do not use the branch.

## Conventions

These are fixed names and repository settings, the same for every project.
publish checks each one it can read and fails the preflight when one is not
met (release-workflow conventions are checked when the release is
verified). None is configurable, except which branch is the
[release branch](#release-branch).

### Git and the local clone

| Convention | Checked by |
| --- | --- |
| Releases are cut from the **release branch**: `-branch <name>`, else `preflight.branch`, else the release repository's default branch on GitHub (`main`, `master` or any name); see [Release branch](#release-branch). It exists on GitHub, is checked out, and `HEAD` = `origin/<release branch>` = the branch on GitHub. A local `refs/remotes/origin/HEAD` naming another branch than the default branch (a renamed default branch, a stale clone) fails the preflight: `git remote set-head origin --auto` updates it. When a branch cannot be read or is not a branch name (no network, no access, a tag, a SHA, a refspec), nothing that depends on it is checked and nothing is tagged | preflight `default branch`, `release branch`, `branch`, `in sync with origin`, `repository` |
| The remote **`origin`** (fetch and push URL) is the `release.github` repository on github.com | preflight `origin`, `origin push url` |
| Run from the repository root, with `git`, `gh` (authenticated: `gh auth status`) and GNU Make | preflight `git repository`, `gh auth`, `generic preflight` |

### hephaestus.yaml

| Convention | Why |
| --- | --- |
| `release.github.owner` and `name` are set | publish releases to one known repository |
| `release.github.draft` is false | a draft is never published |
| `release.github.include` lists only `archive` and `checksum` (or is unset) | publish verifies exactly the archives and the checksum file |
| Archive names do not depend on the build (no templates known only after building) | the expected assets are computed before the tag is pushed |
| `release.github.prerelease: auto` (the default) or explicit values that match the versions you tag | a prerelease is never "latest"; a stable release becomes "latest" unless `make_latest: "false"` |
| The release has notes (`generate_notes: true` or `notes_file`) | an empty release body fails the verification |
| `brews` are written in hephaestus.yaml, not imported with `brews_from` | publish must see every formula to verify the tap |
| Each formula is published by pull request (`repository.pull_request.enabled: true`), without `url_template`, to the directory Homebrew reads on the tap (`Formula/` if the tap has it, else `HomebrewFormula/`, else the root) | the formula pull request is verified against the release |
| `skip_upload: auto` when prereleases must not update the tap; a stable version must publish every formula | the tap is checked untouched for a prerelease and updated for a stable release |
| `hephaestus preflight --tag vX.Y.Z --strict` passes (for example `RELEASE_NOTES.md` or the changelog names the version, when configured) | publish runs it as an additional gate |

### Workflows

| Convention | Checked by |
| --- | --- |
| **`.github/workflows/ci.yml`** runs on `push` to the release branch, and its push run on the release branch for exactly the commit being tagged succeeded (a run on another branch, such as the default branch, or one started through `workflow_call` does not count) | preflight `ci on commit` |
| `ci.yml` has an aggregating job **`ci-gate`** that `needs` every other job and fails unless all succeeded (`if: always()`); it is the check the release branch's ruleset requires | preflight `<release branch> ruleset` |
| **`.github/workflows/release.yml`** is active, runs on `push` of tags `v*`, and is the only workflow that publishes | preflight `release workflow`; the run is found by file, `push` event, tag and commit |
| Its publishing job runs in environment **`release`** | preflight `environment` |
| It attests **every** asset (each archive and the checksum file) with `actions/attest-build-provenance`, in that workflow, on a GitHub-hosted runner | verification `attestations` (`gh attestation verify --signer-workflow <owner>/<repo>/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z --source-digest <commit> --deny-self-hosted-runners`) |
| It uploads no other release assets | verification `assets` |
| A step with `id: plan` runs the action with `command: plan` (or `hephaestus publish plan "$GITHUB_REF_NAME"`, writing its output to `$GITHUB_OUTPUT`) | the tap steps depend on it |
| The two steps that use the tap token are named exactly **`Rehearse the formula and tap pull request (read-only)`** and **`Generate the formula and open a tap pull request`**, both with `if: steps.plan.outputs.tap == 'publish'`, and no other step gets the token | verification `tap steps`: both skipped when the tap is skipped (the token was not even passed), both succeeded when it is published. Without `brews` the steps may be left out |

The release job's plan and tap steps, with the action (no Go needed). The
action's `plan` command also refuses a prerelease whose plan would update
the tap:

```yaml
      - name: Plan the release
        id: plan
        uses: magicdrive/hephaestus@<full-commit-sha>
        with:
          command: plan
      - name: Rehearse the formula and tap pull request (read-only)
        if: steps.plan.outputs.tap == 'publish'
        uses: magicdrive/hephaestus@<full-commit-sha>
        with:
          command: run
          stages: formula,tap
          dry-run: "true"
          github-token: ""
          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
      # ... attest every asset, then publish the release ...
      - name: Generate the formula and open a tap pull request
        if: steps.plan.outputs.tap == 'publish'
        uses: magicdrive/hephaestus@<full-commit-sha>
        with:
          command: run
          stages: formula,tap
          github-token: ""
          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

### Repository settings

| Setting | Required value | Checked by |
| --- | --- | --- |
| GitHub Actions | enabled; the repository is not archived | preflight `actions`, `repository` |
| Ruleset on the release branch | **active** branch ruleset covering it (`refs/heads/<branch>`, a pattern such as `refs/heads/release/*`, `~ALL`, or `~DEFAULT_BRANCH` when it is the default branch) with a required status check **`ci-gate`**. A ruleset on the default branch does not protect a release branch with another name: without one on the release branch nothing is tagged | preflight `<branch> ruleset` (e.g. `main ruleset`) |
| Ruleset on tags | **active** tag ruleset whose conditions cover the tag (e.g. `refs/tags/v*`) with the rules **deletion** and **update**, and no active rule that blocks **creation** of the tag. Bypass actors are reported as a warning | preflight `tag ruleset` |
| Environment `release` | deployment branches and tags: **selected** policies, **tags only** (no branch policy, not "protected branches"), one matching the version (e.g. `v*`); required reviewers are optional | preflight `environment` |
| Secret in `release` (with `brews`) | **`HOMEBREW_TAP_GITHUB_TOKEN`**: write access (contents, pull requests) to the tap only. Only its name is checked; whether it can write is checked by the workflow's read-only rehearsal before the release is published | preflight `tap token secret` |

## Safety rules

- **The tag push is the only release trigger and cannot be taken back.**
  publish pushes exactly one annotated tag, never forced, after the preflight
  and an explicit confirmation (default **No**; `YES=1` on the make command
  line or `-yes` for a non-interactive run). It never moves or deletes a tag,
  locally or on `origin`, and never publishes, edits or deletes a release.
- Right before the push, the release branch (re-fetched `origin/<branch>`, and
  the branch on GitHub) and the remote tag are checked again (they may have
  changed while the prompt was open). A failure before the
  push leaves at most a local tag, which publish does not delete for you.
- Its only writes are that tag and a fetch of `origin/<release branch>` (no tags).
  Every GitHub access is a read through `gh`; secrets are read by name only.
- The release run is identified by workflow file, `push` event, the tag, the
  tagged commit and a creation time after the push — never "the latest
  run".
- A lost connection, Ctrl-C or a timeout stops the **monitoring**, not the
  release: the outcome is reported as unknown (exit `3`), never as failed or
  "nothing was published".
- The formula pull request is verified (only the formula changes, every URL
  and SHA-256 is this release's) and **never merged** by publish: review and
  merge it on the tap.
- Two publish processes in one clone are prevented by a lock file in the git
  directory (`hephaestus-publish.lock`).

## Recovery

Interrupting publish (Ctrl-C, a lost network, a timeout) stops the
monitoring only; the release keeps running on GitHub:

```sh
make publish-status v1.2.3   # tag, run, release, assets, attestations, tap (read-only)
make publish-watch v1.2.3    # find and follow the run again, then verify
```

Running `make publish` again for a version whose tag exists is refused by the
preflight; use `publish-status` and `publish-watch` instead.

| Where it stopped | What exists | What to do |
| --- | --- | --- |
| Preflight or confirmation | nothing | fix the failed check, run `make publish vX.Y.Z` again |
| Tag creation or the re-check before the push | a local tag only | `git show vX.Y.Z`, then `git tag -d vX.Y.Z` and start again |
| Push | a local tag; maybe the remote one | `make publish-status vX.Y.Z`: if the tag is on `origin`, `make publish-watch vX.Y.Z`; otherwise as above |
| Run not found / monitoring stopped | the tag on `origin` | `make publish-watch vX.Y.Z` (or the Actions page); nothing is wrong with the release yet |
| Run failed before the release step | the tag; no release | transient: `gh run rerun <id> --failed`; code fix: merge it and release a **new version** (e.g. the next `-rc.N`) — never move or delete the tag |
| Run failed during the release step | a **draft** release | re-run the failed jobs: the draft is resumed, identical assets are skipped |
| Run failed after publishing | a published release | do not delete it or replace assets; fix forward with a new version |
| Verification failed after a green run | a published release | investigate the failed check (`make publish-status vX.Y.Z`); never "fix" a published release in place |

`hephaestus status --tag vX.Y.Z` ([recovery.md](recovery.md)) inspects any
release, also without the conventions above.
