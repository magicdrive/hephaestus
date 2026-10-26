# Release preflight

`hephaestus preflight` checks, **before you push a release tag**, that the
release can safely run: the local git state, the GitHub repository's release
settings, CI on the exact commit, the Hephaestus configuration for that tag,
and the Homebrew tap. It never changes anything.

```sh
hephaestus preflight --tag v1.2.0
hephaestus preflight --tag v1.2.0-rc.1 --format json --strict
```

Run it from a clone of the release repository, on the release branch, with
a GitHub token in `GITHUB_TOKEN` (or `GH_TOKEN`) for the checks that need
one. It is independent of the language you build in: it reads git, GitHub
and `hephaestus.yaml`.

## Usage

| Flag | Default | Meaning |
| --- | --- | --- |
| `--tag` | required | The tag you are about to push, e.g. `v1.2.0` |
| `--config` | `hephaestus.yaml` (or `.yml`, `.hephaestus.yaml`, `.hephaestus.yml`) | Configuration file. An invalid or missing file is reported by `config.load`; git and GitHub checks still run |
| `--repo` | `release.github.owner/name`, then the git remote | `owner/name` of the release repository |
| `--format` | `text` | `text` or `json` (JSON only on stdout; nothing else is written there) |
| `--strict` | off | Treat `WARN` and `UNKNOWN` as failures |
| `--offline` | off | Skip every check that needs the network (GitHub API, `git ls-remote`) |
| `--remote` | `preflight.remote`, then `origin` | Git remote releases are pushed to |
| `--branch` | `preflight.branch`, then the GitHub default branch | Branch releases are tagged on |
| `--workflow` | `preflight.workflow` | Release workflow file name (e.g. `release.yml`); when set it must exist and be active |
| `--environment` | `preflight.environment` | Environment of the publishing job; when set its deployment policy is checked and secrets are looked up in it |
| `--secret NAME` | `preflight.secrets` | Secret names that must exist (repeatable) |
| `--tap-token-secret` | `preflight.tap_token_secret`, then `HOMEBREW_TAP_GITHUB_TOKEN` | Secret name of the tap token, required when a formula is published to a tap |
| `--skip ID` | `preflight.skip` | Check IDs to skip (repeatable or comma-separated) |
| `--report FILE` | none | Also write the JSON report to FILE (with any `--format`) |
| `--token-env` | `GITHUB_TOKEN` | Environment variable holding the token (`GH_TOKEN` when empty) |

The same settings can live in the configuration, so CI needs only `--tag`:

```yaml
preflight:
  workflow: release.yml
  environment: release
  secrets: [SIGNING_KEY]
  changelog: CHANGELOG.md
  skip: [github.ruleset.branch]
```

In GitHub Actions (`GITHUB_ACTIONS=true`) the text format also emits an
annotation for every `FAIL`, `WARN` and `UNKNOWN` and appends a summary
table to the job summary; the [action](github-action.md#release-preflight)
runs it with `command: preflight`.

## Results

| Status | Meaning |
| --- | --- |
| `PASS` | Checked, and fine |
| `FAIL` | Checked, and the release must not be tagged as things are |
| `WARN` | Checked; a risk or a weaker setup, not a blocker (for example no tag ruleset) |
| `SKIP` | Not applicable (no tap for a prerelease, nothing configured), skipped by `--skip`, `--offline`, or because a prerequisite did not pass |
| `UNKNOWN` | Could not be checked: missing permission, rate limit, network or server error. Never reported as `FAIL` |

The overall status is `fail` if any check failed, else `unknown` if any
could not be made, else `warn` if any warned, else `pass`.

| Exit code | When |
| --- | --- |
| `0` | No `FAIL` (and with `--strict`, no `WARN` or `UNKNOWN`) |
| `1` | A `FAIL`; with `--strict` also a `WARN` or `UNKNOWN` |
| `2` | Invalid arguments (missing `--tag`, unknown check ID, ...) |
| `3` | No `FAIL`, but some checks are `UNKNOWN` (without `--strict`) |

Use `--strict` in automation that must not proceed on partial knowledge.

## Checks

IDs are stable; they appear in the JSON report and in `--skip`.

| ID | Severity | What it checks | Needs |
| --- | --- | --- | --- |
| `git.repository` | error | Inside a git work tree, HEAD has a commit | — |
| `git.remote` | error | The remote's fetch and push URLs are the release repository | local git |
| `git.worktree` | error | No uncommitted or untracked paths | local git |
| `git.branch` | error | On the release branch, not detached | local git |
| `git.head.pushed` | error | HEAD is exactly the remote release branch (the commit CI tested) | `git ls-remote` |
| `tag.format` | error | The tag is valid; WARN when it is not SemVer | — |
| `tag.local` | error | The tag does not exist locally | local git |
| `tag.remote` | error | The tag does not exist on the remote | `git ls-remote` |
| `tag.order` | warning | Newer than the highest released `vX.Y.Z` | `git ls-remote` |
| `config.load` | error | `hephaestus.yaml` loads and validates | — |
| `config.assets` | error | Planned archive and checksum names are valid and unique (case-insensitively, as GitHub compares them) | — |
| `config.release` | error | GitHub release settings for this tag (prerelease, `draft: true` warns) | — |
| `config.release-notes` | error | `release.github.notes_file` (and `preflight.changelog`, if set) exists, is not empty and names the tag or its version as a whole version: `v1.2.0` is not satisfied by `v1.2.0-rc.1` or `v1.2.01`, so notes left from an earlier version or a candidate are caught | — |
| `config.formula` | error | Each formula's tap publication resolves for this tag (skipped for prereleases with `skip_upload: auto`) | — |
| `github.repository` | error | The repository exists and is not archived | GitHub |
| `github.release` | error | No release or draft exists for the tag (drafts need a token with push access to be seen; without one this warns) | GitHub |
| `github.ci` | error | The latest run of every check on HEAD succeeded, nothing is pending, and every check required by the branch ruleset (from the app it names, if any) ran and succeeded; see [Check runs](#check-runs) | GitHub |
| `github.workflow` | error | The release workflow exists and is active (when configured) | GitHub |
| `github.environment` | error | The environment exists and deploys **only from tag policies**, one of which matches the tag; a branch policy (even `v*`) fails | GitHub |
| `github.secrets` | error | Required secret names exist: in the configured environment, else in any environment, else in the repository. The tap token is required only when a formula is published for this tag | GitHub |
| `github.ruleset.branch` | warning | An active ruleset protects the release branch and requires a status check (`~DEFAULT_BRANCH` covers only the repository's default branch, not a configured release branch with another name) | GitHub |
| `github.ruleset.tag` | error | An active tag ruleset forbids deleting and updating the tag (WARN when missing); a rule blocking **creation** fails | GitHub |
| `tap.repository` | error | Each tap a formula is published to exists and is not archived | GitHub |
| `tap.layout` | error | The formula goes where Homebrew reads formulae (`Formula/`, `HomebrewFormula/`, else the root) | GitHub |
| `tap.branch` | error | The pull request branch does not exist yet | GitHub |
| `tap.pull-request` | error | No open pull request from that branch (a closed one warns) | GitHub |

A check whose prerequisite did not pass is `SKIP` with the reason, so one
root cause is reported once.

### Check runs

A commit often has several check runs with the same name: re-run attempts,
the same workflow run again (a release re-run, a second tag on one commit),
or another app reporting a check of that name. `github.ci` reads every run
(`filter=all`; the API's default `filter=latest` selects by `completed_at`
and can hide a newer run that is still in progress) and decides
independently of the order the API returns them in:

- runs are grouped by app and name; each group is decided by its **latest**
  run: the one with the highest ID (IDs grow with creation, so a re-run
  attempt is newer than the run it replaces), else the latest
  `started_at`;
- a newer run that is queued or in progress makes the check pending (FAIL
  "still running"), never hidden behind an older success; an older failure
  does not outlive a newer success;
- runs of different apps are different checks, and each must succeed; a
  required check that names an app (`integration_id`) is satisfied only by
  that app's runs;
- when neither the ID nor `started_at` tells two runs with different
  outcomes apart, the check is `UNKNOWN`, never guessed.

The evidence lists every group with its latest run ID and how many older
runs it superseded.

## JSON report

```json
{
  "schema_version": 1,
  "tool": "hephaestus",
  "tool_version": "v0.2.0",
  "repository": "acme/tool",
  "tag": "v1.2.0",
  "version": "1.2.0",
  "prerelease": false,
  "commit": "b2c5f2808c69e2858455e16e0438016ceb9226db",
  "strict": false,
  "offline": false,
  "status": "fail",
  "summary": { "FAIL": 1, "PASS": 24 },
  "checks": [
    {
      "id": "github.environment",
      "group": "github",
      "title": "Environment deploys only from release tags",
      "status": "FAIL",
      "severity": "error",
      "message": "release also deploys from branches v*: a branch with that name could read the release secrets",
      "evidence": { "environment": "release", "policies": ["branch:v*"] },
      "remediation": "remove the branch policies; keep only tag policies"
    }
  ]
}
```

`checks` lists every check in the order of the table above. `evidence` is
what the decision was based on; it never contains a secret value, a token or
credentials embedded in a remote URL. Fields are added, never renamed, within
a schema version.

## Permissions

The preflight only reads. A fine-grained token with **read** access to the
release repository (Contents, Metadata, Actions, Environments, Administration
for rulesets, Secrets for secret names) and to the tap (Contents, Pull
requests) covers every check; `GITHUB_TOKEN` in a workflow with
`contents: read` and `actions: read` covers most of them. Without access a
check is `UNKNOWN` with a hint, not `FAIL`.

| Checks | GitHub endpoint (all `GET`) |
| --- | --- |
| `github.repository`, `tap.repository` | `/repos/{o}/{r}` |
| `github.release` | `/repos/{o}/{r}/releases` |
| `github.ci` | `/repos/{o}/{r}/commits/{sha}/check-runs?filter=all`, `/commits/{sha}/status` |
| `github.workflow` | `/repos/{o}/{r}/actions/workflows/{file}` |
| `github.environment` | `/repos/{o}/{r}/environments/{env}`, `.../deployment-branch-policies` |
| `github.secrets` | `/repos/{o}/{r}/environments`, `/repos/{o}/{r}/environments/{env}/secrets`, `/repos/{o}/{r}/actions/secrets` (names only; secret values are not retrievable through the API) |
| `github.ruleset.*`, `github.ci` | `/repos/{o}/{r}/rulesets`, `/rulesets/{id}` |
| `tap.layout`, `tap.branch`, `tap.pull-request` | `/repos/{o}/{t}/contents`, `/git/ref/heads/{branch}`, `/pulls` |

Each endpoint is read once per run and the answer is shared by the checks
that need it.

## Read-only guarantee

- **GitHub**: the preflight holds only an interface of `GET` methods; it has
  no way to create, update or delete anything. Tests run it against a
  recording server and fail on any other method.
- **git**: only `rev-parse`, `symbolic-ref --quiet`, `status --porcelain`,
  `remote get-url` and `ls-remote` can run; anything else is refused before
  git starts. Every command runs with `--no-optional-locks` (so `git status`
  does not even refresh the index) and with prompts disabled. There is no
  `git fetch`: the remote state comes from `git ls-remote`. Tests snapshot
  the repository and its remote and fail on any change.
- **No hooks**: the preflight runs no repository commands (see below).
- **Secrets**: only secret names are requested; the token is never printed,
  and every output passes through the same redactor as the other commands.

## Repository-specific checks

Checks that only make sense for one repository (a man page version, a
changelog entry, a version constant in the source) do not belong in the
generic preflight, which promises to run no repository code. Run them next
to it, as separate gates:

```sh
scripts/check-version.sh "$TAG"            # the repository's own rules
hephaestus preflight --tag "$TAG" --strict  # the generic release checks
```

or consume the JSON (`--format json`) in your own script, keyed by check ID.

## Recommended settings

| Setting | Value |
| --- | --- |
| Environment of the publishing job | Deployment policy with **tag** rules only, e.g. `v*`; the tap token secret stored there |
| Branch ruleset on the release branch | Pull request required, the CI gate check required, no force pushes, no deletion |
| Tag ruleset on `refs/tags/v*` | `deletion` and `update` rules; no `creation` rule |
| Workflow permissions | Read by default; the publishing job requests `contents: write` (and `id-token`/`attestations` for provenance) |
| Immutable releases | On: Hephaestus publishes only complete, verified drafts |

## In CI

```yaml
- name: Release preflight
  env:
    GITHUB_TOKEN: ${{ github.token }}
  run: |
    go install github.com/magicdrive/hephaestus@v0.2.0
    hephaestus preflight --tag "$TAG" --strict --format json > preflight.json
```

Secret names, environments and rulesets usually need more than the default
`GITHUB_TOKEN`; without them those checks are `UNKNOWN`, which `--strict`
turns into a failure. Run the preflight where the token can read them, or
`--skip` the checks you accept not to verify.

## Troubleshooting

- **`git.branch` UNKNOWN "release branch unknown"**: offline or no GitHub
  access, and `refs/remotes/origin/HEAD` is unset: pass `--branch`, set
  `preflight.branch`, or run `git remote set-head origin --auto`.
- **`git.head.pushed` FAIL**: you are ahead of or behind the remote branch;
  release only the commit CI tested.
- **`github.environment` FAIL "also deploys from branches"**: the deployment
  policy has a branch rule; replace it with a tag rule. A branch named like
  your tags could otherwise read the release secrets.
- **`github.secrets` UNKNOWN 403**: listing secret names needs admin access
  or the Secrets read permission.
- **`github.ruleset.*` UNKNOWN 404/403**: rulesets of private repositories on
  some plans are not readable; check them in the UI and `--skip` them.
- **Exit code 3**: some checks are `UNKNOWN`; fix the access or use
  `--strict` to make that a failure.
