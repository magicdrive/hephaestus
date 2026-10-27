# Release protection

How the magicdrive repositories keep an unreviewed, untested or older change
from reaching `main`, a release tag, or the Homebrew tap. This page is the
owner's reference for the repository settings; every setting here is applied
by the owner, never by Hephaestus or an agent.

The formula downgrade guard is described in
[tap-version-guard.md](tap-version-guard.md); the merge gate in
[pr-gate.md](pr-gate.md); the general threat model in
[security/threat-model.md](security/threat-model.md).

## Architecture

```text
 pull request ──► CI workflow (jobs ... ──► ci-gate)          tool repositories
                        │ required check "ci-gate" (Actions, 15368)
                        ▼
                 branch ruleset of main ──► pr-gate -merge (pinned head + base SHA,
                        │                    serialized by a lock file)
                        ▼
                      main ──► tag v* (tag ruleset: no update, no deletion)
                                  │
                                  ▼
                         release workflow ──► Hephaestus tap stage
                                                  │ skips a version older than the
                                                  │ base or an open formula PR
                                                  ▼
                                   formula pull request on homebrew-tap
                                                  │ required check "version-guard"
                                                  │ (strict: up to date with main)
                                                  ▼
                                         homebrew-tap main
```

| Layer | Stops | Where |
| --- | --- | --- |
| `ci-gate` job | a merge while any CI job failed, was skipped or cancelled | each repository's CI workflow |
| Branch ruleset | direct pushes, force pushes, deletion of `main`, a merge without `ci-gate` | repository settings |
| `pr-gate` | a merge of a head or base other than the reviewed one, an unknown or pending check; two merges at once (lock file) | the merging operator |
| Tag ruleset | moving or deleting a published `v*` tag | repository settings |
| Tap stage | proposing a formula older than the tap or an open formula PR | Hephaestus release run |
| `version-guard` | merging a formula change that lowers a version | homebrew-tap CI + ruleset |

## Threat model

| Threat | Mitigation | Residual risk |
| --- | --- | --- |
| A failing or skipped job is merged | `ci-gate` treats everything but `success` as failure; it is the required check | A job missing from `ci-gate.needs` is not covered: add every new job to `needs` |
| A check with the same name from another source | `integration_id: 15368` accepts only GitHub Actions | Any workflow in the repository can report a `ci-gate` job; keep the name unique |
| A pull request edits the workflow so that `ci-gate` passes | `pull_request` runs the PR's own workflow file; only the owner merges, after review | Review workflow changes; a required check cannot prevent this alone |
| A stale green check (base moved) | `pr-gate` requires the head to contain the base SHA it was given; strict checks on the tap | Repositories with strict off rely on `pr-gate` |
| A release tag is moved or deleted | Tag ruleset: `update`, `deletion`, `non_fast_forward` | An admin can disable the ruleset |
| The tap is downgraded by merge order | tap stage skip + `version-guard` + strict | A bypass actor or a disabled ruleset |
| A compromised action or tool version | Actions pinned by full commit SHA; Renovate updates with review | CI jobs on `*-latest` change image underneath; the release `build`/`release` jobs are pinned (see [Runners and Go toolchain](#runners-and-go-toolchain)) |
| A workflow token with more scope than the job needs | top-level `permissions: contents: read` in every workflow; write scopes only on the job that needs them, with a comment | A repository whose default workflow permission is `write` still grants it to a workflow that omits `permissions:` (see [Workflow token permissions](#workflow-token-permissions)) |
| An admin bypass | `bypass_actors: []` in every ruleset | The owner can edit rulesets; that is the rollback path |

## Required checks

Values for the `required_status_checks` rule. `integration_id` 15368 is
GitHub Actions.

| Repository | Branch | Check | integration_id | Strict | Status (2026-10-27) |
| --- | --- | --- | --- | --- | --- |
| hephaestus | main | `ci-gate` | 15368 | true | set |
| ark | main | `ci-gate` | 15368 | false | set |
| enma | main | `ci-gate` | 15368 | false | set |
| goreg | main | `ci-gate` | 15368 | false | set |
| kjak | main | `ci-gate` | 15368 | false | set |
| git-parallel | main | `ci-gate` | 15368 | false | set |
| t | master | `ci-gate` | 15368 | true | set |
| kirke | main | `ci-gate` | 15368 | false | **no ruleset** |
| aqua-registry | main | `ci-gate` | 15368 | false | **no ruleset**; `ci-gate` reports on main |
| homebrew-tap | main | `version-guard` | 15368 | **true** | **no ruleset**; the workflow is not on main yet (no check-run) |

Why one aggregate check: matrix jobs report names such as `test
(ubuntu-latest)` that change when a runner is added or renamed; a required
check that no longer runs blocks every pull request. `ci-gate` is the only
name a ruleset references, so jobs can be added, renamed or split without a
settings change, as long as `ci-gate.needs` lists them. The required check is
the **job** name (or its `name:`), not the workflow name: renaming the
workflow is safe, renaming the `ci-gate` job is not.

Strict ("require branches to be up to date"): on the tap it is required,
because a green `version-guard` from an older base does not prove the merge is
not a downgrade. On the tool repositories `pr-gate` already refuses a head that
does not contain the current base (`base.up_to_date`), so strict adds only the
cost of updating every open pull request after each merge; keeping it `false`
for new rulesets (kirke, aqua-registry) matches the majority. hephaestus and
`t` keep `true`.

## Ruleset configuration

Apply in this order, per repository:

1. Merge the workflow that defines the check (`ci-gate`, `version-guard`).
2. Wait until the check has succeeded at least once on the default branch
   (`gh api "repos/$R/commits/main/check-runs?check_name=ci-gate" --jq '.check_runs[0].conclusion'`).
   Only then is it selectable in the UI and known to exist.
3. Create the branch ruleset with the check required.
4. Create the tag ruleset.
5. Validate (below).

Requiring a check before a workflow reports it blocks every pull request,
including the one that would add it.

### UI

Settings, Rules, Rulesets, New ruleset, New branch ruleset:

1. Name `protect main`; Enforcement status **Active**; Bypass list empty.
2. Target branches: Add target, **Include default branch**.
3. Enable **Restrict deletions**, **Block force pushes**, **Require a pull
   request before merging** (0 approvals).
4. Enable **Require status checks to pass**, Add checks, `ci-gate` (source
   GitHub Actions; `version-guard` on homebrew-tap). Set **Require branches
   to be up to date before merging** per the table.
5. Create.

New ruleset, New tag ruleset: name `protect release tag`, Active, bypass
empty, target `v*` (Include by pattern), enable **Restrict updates**,
**Restrict deletions**, **Block force pushes**. Do **not** enable
**Restrict creations**: the release workflow creates the tag
(`hephaestus preflight` fails on a rule that blocks creation).

### API

```sh
R=magicdrive/kirke CHECK=ci-gate STRICT=false
gh api -X POST "repos/$R/rulesets" --input - <<JSON
{
  "name": "protect main",
  "target": "branch",
  "enforcement": "active",
  "bypass_actors": [],
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "rules": [
    {"type": "deletion"},
    {"type": "non_fast_forward"},
    {"type": "pull_request", "parameters": {
      "required_approving_review_count": 0,
      "dismiss_stale_reviews_on_push": false,
      "require_code_owner_review": false,
      "require_last_push_approval": false,
      "required_review_thread_resolution": false}},
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": $STRICT,
      "required_status_checks": [{"context": "$CHECK", "integration_id": 15368}]}}
  ]
}
JSON

gh api -X POST "repos/$R/rulesets" --input - <<'JSON'
{
  "name": "protect release tag",
  "target": "tag",
  "enforcement": "active",
  "bypass_actors": [],
  "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
  "rules": [{"type": "deletion"}, {"type": "non_fast_forward"}, {"type": "update"}]
}
JSON
```

For homebrew-tap use `CHECK=version-guard STRICT=true`; its tag ruleset is
optional (the tap has no tags).

Validation:

```sh
gh api "repos/$R/rules/branches/main" --jq '.[].type'
gh api "repos/$R/rulesets" --jq '.[] | {id, name, target, enforcement}'
```

Then open a trivial pull request: the merge box must list the required
check, and `pr-gate` (dry run) must report `required.read` with it.

## Exact configuration (verified 2026-10-27)

Read with the API on 2026-10-27; nothing was changed.

| Repository | Rulesets today | Required check | Check-run on the default branch | strict | bypass |
| --- | --- | --- | --- | --- | --- |
| homebrew-tap | none (`GET repos/$R/rulesets` is `[]`, branch not protected) | `version-guard` | none yet: the tap has no workflow on `main` | **true** (to set) | `[]` (to set) |
| kirke | none | `ci-gate` | `ci-gate`, app id 15368 (`github-actions`), success | false (to set) | `[]` (to set) |
| aqua-registry | none | `ci-gate` | `ci-gate`, app id 15368 (`github-actions`), success | false (to set) | `[]` (to set) |
| hephaestus (reference) | `Protect main` (branch), `Protect release tags` (tag), both `active` | `ci-gate`@15368 | yes | true | `[]`, `current_user_can_bypass: never` |

- `gh api apps/github-actions` returns `{"id": 15368, "slug": "github-actions", "owner": "github"}`: `integration_id: 15368` admits only check-runs created by GitHub Actions.
- The check name is the job's name as reported in the check-run (`name` of `GET repos/$R/commits/<branch>/check-runs?check_name=ci-gate`), not the workflow's name.
- homebrew-tap: step 2 of [Ruleset configuration](#ruleset-configuration) is not met; the ruleset must wait until `version-guard` (with `-strict`) has reported on `main`.

Verify each value after applying:

```sh
R=magicdrive/kirke
id=$(gh api "repos/$R/rulesets" --jq '.[] | select(.target=="branch") | .id')
gh api "repos/$R/rulesets/$id" --jq '{enforcement, bypass_actors, current_user_can_bypass,
  include: .conditions.ref_name.include,
  checks: [.rules[] | select(.type=="required_status_checks") | .parameters]}'
# expect: enforcement "active", bypass_actors [], current_user_can_bypass "never",
#         required_status_checks [{"context":"ci-gate","integration_id":15368}],
#         strict_required_status_checks_policy false (true on homebrew-tap)
gh api "repos/$R/commits/main/check-runs?check_name=ci-gate" --jq '.check_runs[0] | {name, app: .app.id}'
```

### Availability on a user-owned public repository

| Feature | Available | Evidence |
| --- | --- | --- |
| Branch ruleset | yes | hephaestus `Protect main`, active |
| Tag ruleset | yes | hephaestus `Protect release tags`, active |
| Required status checks with `integration_id` | yes | hephaestus `ci-gate`@15368 |
| Strict (up to date before merging) | yes | hephaestus `strict_required_status_checks_policy: true` |
| Empty bypass list | yes | hephaestus `bypass_actors: []`, `current_user_can_bypass: never` |
| Bypass actors | not verified | no ruleset of these repositories has one; the owner would see the choices in the UI. None is wanted. |
| Merge queue (`merge_queue` rule) | no (organization-owned repositories only) | see [Merge queue](#merge-queue); not re-tested, since it needs a settings change |

### Where a ruleset applies

With `bypass_actors: []`, the owner (repository admin) is subject to the rules like everyone else: `current_user_can_bypass` is `never`. Unlike classic branch protection, there is no "include administrators" switch.

| Path | `pull_request` rule | `required_status_checks` | `non_fast_forward` / `deletion` |
| --- | --- | --- | --- |
| Merge button (UI) | satisfied by the PR | enforced (strict: head must contain the base) | n/a |
| Merge through the API (`PUT pulls/{n}/merge`, `pr-gate -merge`) | satisfied | enforced, as in the UI | n/a |
| Direct `git push` to `main` | rejected (changes must go through a PR) | rejected | force push and deletion rejected |
| Admin (owner) | same as above while `bypass_actors` is empty | same | same |
| Admin editing or disabling the ruleset | not prevented: this is the [rollback](#rollback-policy) and [emergency](#emergency-procedure) path | | |

The last row is the residual risk: the owner account itself (and a token with `administration: write`) can turn protection off. Keep such tokens out of workflows.

## Merge queue

A merge queue is available only to repositories owned by an organization.
All magicdrive repositories are owned by a user account, so the
`merge_queue` rule cannot be used, and no workflow needs a `merge_group`
trigger today. If the repositories move to an organization, add
`merge_group:` to each CI workflow **before** enabling the queue (otherwise
the required check never reports on queue entries and every entry times
out); the tap workflow already describes its `merge_group` mode.

## Rollback policy

- A ruleset is never deleted to unblock a merge. Set its enforcement to
  `disabled` (`gh api -X PUT "repos/$R/rulesets/<id>"` with the whole
  ruleset and `"enforcement": "disabled"`, or the UI), merge the fix, and set
  it back to `active` in the same session.
- A required check that no longer exists (job renamed): restore the job
  name in a pull request; if that pull request itself cannot merge, disable
  the ruleset for that one merge as above.
- A released tag is never moved. A bad release is followed by a new patch
  version; see [recovery.md](recovery.md).
- An intentional formula rollback needs the tap owner's approval comment for
  the pull request head plus the label `formula-rollback`; the label alone
  allows nothing (see [tap-version-guard.md](tap-version-guard.md#intentional-rollback)).

## Failure recovery

| Symptom | Cause | Action |
| --- | --- | --- |
| "Expected — Waiting for status to be reported" | the required check is not produced (renamed job, workflow not triggered, path filter) | fix the workflow; check `gh api repos/$R/commits/<sha>/check-runs` |
| `ci-gate` failed but every job is green | a job was skipped or cancelled (concurrency) | re-run the workflow |
| Branch is out of date (strict) | base moved | Update branch, wait for CI |
| `pr-gate` exit 3, lock held | another merge is running | wait; never remove a held lock |
| `version-guard` failed | one of: the PR lowers a formula's version; with `-strict` also a formula with no or a non-semantic version, versions that cannot be compared, the same version with other artifacts (url, sha256, mirror), a url naming another version than the formula, a removed formula, a formula that is not a regular file or an unsafe path | read the job's annotations; fix the formula, or close the PR. For an intended downgrade, the owner posts the `/formula-rollback` approval for its head, then labels it `formula-rollback` ([tap-version-guard.md](tap-version-guard.md#intentional-rollback)). Never disable the ruleset for it |

## Emergency procedure

For a fix that must land while CI is broken for reasons outside the change
(for example a GitHub outage):

1. Record the reason and the ruleset IDs.
2. Disable the branch ruleset (enforcement `disabled`), not its deletion.
3. Merge the single fix.
4. Re-enable the ruleset immediately and verify with
   `gh api "repos/$R/rules/branches/main"`.
5. Run CI on `main` afterwards and treat its result as the release gate.

Never disable a tag ruleset to move a tag.

**kjak v1.0.0 is not to be released** until the real-send test of kjak
(sending through the real service, not a mock) has passed. No emergency
justifies tagging `v1.0.0` before that; an urgent kjak fix ships as a
`v0.x` patch.

## Owner checklist

| Repository | Setting | Exact value | Reason | Risk | Validation | Rollback |
| --- | --- | --- | --- | --- | --- | --- |
| aqua-registry | merge aqua-registry#4 | `ci-gate` job | one stable check name | none | `ci-gate` green on main | revert PR |
| aqua-registry | branch ruleset | `~DEFAULT_BRANCH`; deletion, non_fast_forward, pull_request (0), required `ci-gate`@15368, strict false, bypass [] | no protection today | blocks PRs if added before step 1 | `rules/branches/main` | enforcement `disabled` |
| aqua-registry | tag ruleset | `refs/tags/v*`; deletion, non_fast_forward, update | tags are the registry's release | none | `rulesets` list | enforcement `disabled` |
| kirke | branch ruleset | as aqua-registry, `ci-gate`@15368, strict false | no protection today; `ci-gate` exists | none (check already reports) | `rules/branches/main` | enforcement `disabled` |
| kirke | tag ruleset | `refs/tags/v*`; deletion, non_fast_forward, update | published tags must not move | none | `rulesets` list | enforcement `disabled` |
| homebrew-tap | branch ruleset | required `version-guard`@15368, strict **true**, deletion, non_fast_forward, pull_request (0), bypass [] | formula downgrade by merge order | each merge requires updating other open PRs | `rules/branches/main` | enforcement `disabled` |
| kjak | release | **do not tag or release `v1.0.0`** until the real-send test has passed | v1.0.0 is a compatibility promise; sending is untested against the real service | a published major cannot be withdrawn | `gh api repos/magicdrive/kjak/git/matching-refs/tags/v1` is `[]` | ship fixes as `v0.x` |
| git-parallel | Actions, General, Workflow permissions | **Read repository contents and packages permissions**; **Allow GitHub Actions to create and approve pull requests** off | the only repository still on `write` / `true`; every other repository is `read` / `false` | none: every workflow already declares `permissions:` | `gh api repos/magicdrive/git-parallel/actions/permissions/workflow` returns `{"default_workflow_permissions":"read","can_approve_pull_request_reviews":false}` | set both back in the same page |
| homebrew-tap | branch ruleset | only after `version-guard` (`-strict`) has reported on `main` | a required check that never reports blocks every PR | — | [Exact configuration](#exact-configuration-verified-2026-10-27) | enforcement `disabled` |
| all | bypass list | empty | a bypass skips every check | none | `rulesets/<id> --jq .bypass_actors` | — |
| all | security settings | see the table below | | | | |


### Security settings

All of these are free for public repositories and need the repository
admin (the owner). State read on 2026-10-27 (`GET repos/$R`
`.security_and_analysis`, `GET repos/$R/vulnerability-alerts` (204 on, 404
off), `GET repos/$R/private-vulnerability-reporting`,
`GET repos/$R/code-scanning/default-setup`):

| Repository | Dependabot alerts | Secret scanning | Push protection | Private vulnerability reporting | CodeQL default setup | Languages for CodeQL |
| --- | --- | --- | --- | --- | --- | --- |
| hephaestus | off | on | on | off | not configured | Go, Actions |
| ark | off | off | off | off | not configured | Go, Actions |
| enma | on | off | off | off | not configured | Go, Actions |
| goreg | on | off | off | off | not configured | Go, Actions |
| kirke | on | off | off | off | not configured | Go, Actions |
| kjak | off | on | on | off | not configured | Go, Actions |
| git-parallel | on | off | off | off | not configured | Actions (shell is not analyzed) |
| t | on | off | off | off | not configured | Actions (shell is not analyzed) |
| aqua-registry | off | on | on | off | not configured | Actions |
| homebrew-tap | off | on | on | off | not configured | none today (Ruby formulae only, no workflow on main) |

Dependabot security updates are off everywhere, which is intended (see
below).

| Feature | Recommendation | Cost | CI load | Overlap with existing workflows | Operational effect |
| --- | --- | --- | --- | --- | --- |
| Dependabot alerts | on everywhere | free | none (runs on GitHub's side from the dependency graph) | `govulncheck` in CI (hephaestus, ark, enma, goreg, kirke, kjak) checks reachable symbols at CI time; alerts also cover new advisories between pushes and every module, reachable or not | alerts in the Security tab and by notification; Renovate's `osvVulnerabilityAlerts` turns them into PRs |
| Secret scanning | on everywhere | free | none | none | alerts for committed provider secrets |
| Push protection | on everywhere | free | none | none | a push containing a known secret is rejected (the pusher can bypass with a reason, which is recorded) |
| Private vulnerability reporting | on for every repository that ships a binary or formula (all) | free | none | complements `SECURITY.md` (present in hephaestus, ark, enma, kjak) | reports arrive as draft security advisories; point `SECURITY.md` at it |
| CodeQL default setup | on for Go repositories (Go + Actions); Actions-only for shell repositories; skip homebrew-tap until it has workflows | free on public repositories (Actions minutes) | one `CodeQL` workflow per push to the default branch and per PR, a few minutes for these sizes, plus a weekly scan | partly overlaps `staticcheck`/`golangci-lint`, `actionlint`/`zizmor`-style workflow checks; CodeQL adds taint/data-flow queries none of them run | not a required check (do not add it to a ruleset: a CodeQL outage would block merges); review alerts in the Security tab |

Owner procedure, per repository `R=magicdrive/<name>`:

```sh
gh api -X PUT "repos/$R/vulnerability-alerts"                       # Dependabot alerts
gh api -X PATCH "repos/$R" \
  -f 'security_and_analysis[secret_scanning][status]=enabled' \
  -f 'security_and_analysis[secret_scanning_push_protection][status]=enabled'
gh api -X PUT "repos/$R/private-vulnerability-reporting"
gh api -X PATCH "repos/$R/code-scanning/default-setup" -f state=configured -f query_suite=default
# leave Dependabot security updates off:
gh api "repos/$R/automated-security-fixes" --jq .enabled              # expect false
```

UI: Settings, Advanced Security (Code security): Dependabot alerts Enable;
Secret Protection / Secret scanning Enable and Push protection Enable;
Private vulnerability reporting Enable; Code scanning, CodeQL analysis,
Set up, Default (keep the detected languages, Enable).

Verification after setting:

```sh
gh api -i "repos/$R/vulnerability-alerts" | head -1                   # HTTP 204
gh api "repos/$R" --jq '.security_and_analysis | {secret_scanning: .secret_scanning.status, push_protection: .secret_scanning_push_protection.status, dependabot_security_updates: .dependabot_security_updates.status}'
gh api "repos/$R/private-vulnerability-reporting" --jq .enabled       # true
gh api "repos/$R/code-scanning/default-setup" --jq '{state, languages}' # configured
gh api "repos/$R/code-scanning/analyses?per_page=1" --jq '.[0] | {tool: .tool.name, created_at}'  # after the first run
```

### Dependabot and Renovate

| Concern | Owner | Notes |
| --- | --- | --- |
| Dependency update PRs (Go modules, GitHub Actions digests, tool versions) | Renovate (`renovate.json`) | one bot, one schedule, grouped PRs |
| Vulnerability detection | Dependabot alerts | dependency graph, GitHub Advisory Database |
| PRs for vulnerable dependencies | Renovate (`osvVulnerabilityAlerts: true`, the "vulnerable packages first" rule) | bypasses the schedule and `minimumReleaseAge` for the fix |
| Dependabot version updates | none | no `.github/dependabot.yml` |
| Dependabot security updates | off | if turned on, Dependabot and Renovate open two PRs for the same advisory (different branches and titles, neither closes the other). If the owner ever turns them on, turn off Renovate's `vulnerabilityAlerts`/`osvVulnerabilityAlerts` in that repository in the same change, so exactly one bot reacts to an advisory |

`.github/dependabot.yml` per repository:

| Repository | File | Effect | Action |
| --- | --- | --- | --- |
| ark | gomod, `open-pull-requests-limit: 0` | inert (opens no PR) | removed (ark#93) |
| goreg | gomod, `open-pull-requests-limit: 0` | inert | removed (goreg#49) |
| kirke | gomod, `open-pull-requests-limit: 10` | Dependabot version updates are kirke's only updater (no `renovate.json`; Renovate has never opened a PR there) | owner decision: install Renovate on kirke, then replace the file with a `renovate.json` like goreg's in one PR. Until then the file stays, otherwise kirke has no updater at all |

### Workflow token permissions

Every workflow declares top-level `permissions:` (normally `contents:
read`); a job that writes declares its own scopes with a comment. With that,
the repository's default workflow permission does not widen any job, but it
still applies to a workflow added later without `permissions:`; so the
default is also set to read.

| Repository | Default (`actions/permissions/workflow`) | Top-level `permissions:` in every workflow | Write scopes |
| --- | --- | --- | --- |
| git-parallel | **write**, can approve PRs **true** | yes (`ci.yml`, `release.yml`: `contents: read`) | `release` job only: `contents: write` (create the release, upload assets), `id-token: write` + `attestations: write` (provenance) |
| all others | read, false | yes | `release` job only (the same three scopes); hephaestus also `id-token: write` on the action-boundary test job and `contents: write` on the release rehearsal's draft-release job |

Effect of the git-parallel setting change: none on the workflows (each job
already receives exactly what it declares). Renovate runs as a GitHub App
and does not use the workflow token. A PR from a fork always gets a
read-only token, whatever the setting. A job that ever needs more than it
declares fails with `Resource not accessible by integration` (HTTP 403):
fail closed; fix by adding the scope to that job with a comment, never by
raising the top level or the default.

## Runners and Go toolchain

The `build` and `release` jobs of every `release.yml` run on
`ubuntu-24.04`, not `ubuntu-latest`: `ubuntu-latest` moves to Ubuntu 26.04
in November 2026 (actions/runner-images#14748), and a release should change
image only through a reviewed PR. GitHub supports at most two GA Ubuntu
images; Ubuntu 22.04 is being retired (deprecation from 2026-09-17,
unsupported by 2027-04-17, actions/runner-images#14254), so 24.04 and 26.04
are the supported pair. CI keeps `-latest` on purpose, so that a 26.04
incompatibility shows up in CI first; then the release jobs move to
`ubuntu-26.04` in one PR, well before 24.04 is announced for deprecation.

| Repository | Release jobs runner | Release Go | Single source |
| --- | --- | --- | --- |
| hephaestus, ark, goreg, kjak | ubuntu-24.04 | `go-version-file: go.mod` (`toolchain` line) | go.mod |
| enma | ubuntu-24.04 | `1.27.3`, equal to `builds[].env.GOTOOLCHAIN` (checked in CI; Renovate updates both in one group) | hephaestus.yaml GOTOOLCHAIN (go.mod's `go 1.25.0` stays the oldest supported Go, which the test job uses) |
| kirke | ubuntu-24.04 | `1.27.3`, equal to GOTOOLCHAIN | hephaestus.yaml GOTOOLCHAIN |
| git-parallel, t | ubuntu-24.04 | no Go build (shell; Go only runs actionlint in CI) | — |

macOS and Windows runners are only used in CI matrices, not in release
jobs, so they are not pinned.

## Publisher and guard contract

The tap stage of a release (the publisher, `internal/tap`, with the
same-version checksum rule of #44) and the tap's required check
(`tap-version-guard -strict`, #46) look at the same formula from two sides.
The publisher decides what a release proposes; the guard decides what may
be merged into the tap, whoever proposed it.

| Case | Publisher (release tap stage) | Guard (`tap-version-guard -strict`, tap CI) |
| --- | --- | --- |
| Same version, identical formula | no-op (`unchanged`), nothing written | not a changed file: passes |
| Same version, sha256 changed for the same url | fails with `ChecksumConflictError` before writing, also in a dry run (base branch, head branch or a trusted open PR); `allow_downgrade` does not help | fails (same version with other artifacts); overridable only by the rollback approval |
| url changed (same version) | not a checksum conflict (urls are the key): proposed | fails (same version with other artifacts); a url naming another version than the formula fails too |
| Rollback (lower version) | skips with a warning ("Tap has a newer version"), run stays green; writes only with `brews[].allow_downgrade` | fails; overridable only by the rollback approval |
| Formula removed | never removes a formula | fails (removed formula); overridable by the rollback approval |
| Invalid / non-semantic version | warns that it cannot compare and updates the formula | fails (no version, non-semantic, or incomparable versions); "no version" is never overridable |
| Platform added at the same version | proposed (a new url is not a checksum change) | fails (same version with other artifacts): a new platform needs a new version, or the rollback approval |
| Fork PR | an open PR from someone else's fork is only warned about: it never skips or fails publishing (#44). PRs from a branch of the tap or of the configured head repository still count | judges the fork PR's content like any other PR (`pull_request` runs with a read-only token); the author is not a criterion |

Differences that matter: the publisher is lenient where a skip or a
proposal is harmless (the merge is still gated), and strict only where
writing would be wrong (checksum conflict). The guard is the fail-closed
gate. Two cases differ in outcome (url change and new platform at the same
version are proposed by the publisher and refused by the guard): such a
release produces a tap PR that cannot merge without the approval. That is
intended; release a new version instead.

Future consolidation: both sides parse formulae (version, url/sha256
pairs, heredocs) in separate code (`internal/tap` and
`cmd/tap-version-guard/strict.go`), so a parser fix has to be made twice
and the two can drift. Moving the guard's rules into `internal/tap` as a
function of (old, new) formula, called by both, would remove that cost; the
publisher would then use the guard's verdict to fail early instead of
proposing a PR that cannot merge. Against it: the guard's job is to be a
small, independent check of the publisher; sharing one parser means a
parser bug passes both. Recommendation: share the parser and the version
comparison (already shared: `tap.CompareVersions`, `tap.ChecksumChanges`),
keep the decision tables separate, and test both against one table of
fixture formulae so drift fails CI. No change is made now.

The rollback approval is being designed (not a label alone: an admin's
approval comment bound to the head SHA, evaluated by a
`pull_request_target` workflow); details in
[tap-version-guard.md](tap-version-guard.md).
