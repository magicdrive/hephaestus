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
| A compromised action or tool version | Actions pinned by full commit SHA; Renovate updates with review | Runner images (`*-latest`) change underneath |
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
| aqua-registry | main | `ci-gate` | 15368 | false | **no ruleset**; the job is added by aqua-registry#4 |
| homebrew-tap | main | `version-guard` | 15368 | **true** | **no ruleset**; the workflow is being added |

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
- A formula downgrade is reverted by a pull request labeled
  `formula-rollback` (see [tap-version-guard.md](tap-version-guard.md)).

## Failure recovery

| Symptom | Cause | Action |
| --- | --- | --- |
| "Expected — Waiting for status to be reported" | the required check is not produced (renamed job, workflow not triggered, path filter) | fix the workflow; check `gh api repos/$R/commits/<sha>/check-runs` |
| `ci-gate` failed but every job is green | a job was skipped or cancelled (concurrency) | re-run the workflow |
| Branch is out of date (strict) | base moved | Update branch, wait for CI |
| `pr-gate` exit 3, lock held | another merge is running | wait; never remove a held lock |
| `version-guard` failed | the PR lowers a formula | close it, or label `formula-rollback` if intended |

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

## Owner checklist

| Repository | Setting | Exact value | Reason | Risk | Validation | Rollback |
| --- | --- | --- | --- | --- | --- | --- |
| aqua-registry | merge aqua-registry#4 | `ci-gate` job | one stable check name | none | `ci-gate` green on main | revert PR |
| aqua-registry | branch ruleset | `~DEFAULT_BRANCH`; deletion, non_fast_forward, pull_request (0), required `ci-gate`@15368, strict false, bypass [] | no protection today | blocks PRs if added before step 1 | `rules/branches/main` | enforcement `disabled` |
| aqua-registry | tag ruleset | `refs/tags/v*`; deletion, non_fast_forward, update | tags are the registry's release | none | `rulesets` list | enforcement `disabled` |
| kirke | branch ruleset | as aqua-registry, `ci-gate`@15368, strict false | no protection today; `ci-gate` exists | none (check already reports) | `rules/branches/main` | enforcement `disabled` |
| kirke | tag ruleset | `refs/tags/v*`; deletion, non_fast_forward, update | published tags must not move | none | `rulesets` list | enforcement `disabled` |
| homebrew-tap | branch ruleset | required `version-guard`@15368, strict **true**, deletion, non_fast_forward, pull_request (0), bypass [] | formula downgrade by merge order | each merge requires updating other open PRs | `rules/branches/main` | enforcement `disabled` |
| all | bypass list | empty | a bypass skips every check | none | `rulesets/<id> --jq .bypass_actors` | — |
| all | security settings | see the table below | | | | |

### Security settings

Free for public repositories. Recommended for every repository:

| Setting | Recommendation | API |
| --- | --- | --- |
| Dependabot alerts | on | `gh api -X PUT repos/$R/vulnerability-alerts` |
| Dependabot security updates | off (Renovate already proposes updates; two bots duplicate PRs) | — |
| Secret scanning | on | `gh api -X PATCH repos/$R -f 'security_and_analysis[secret_scanning][status]=enabled'` |
| Push protection | on | `gh api -X PATCH repos/$R -f 'security_and_analysis[secret_scanning_push_protection][status]=enabled'` |
| Private vulnerability reporting | on | `gh api -X PUT repos/$R/private-vulnerability-reporting` |
| CodeQL default setup | on (actions + language) | `gh api -X PATCH repos/$R/code-scanning/default-setup -f state=configured` |

UI: Settings, Code security (Advanced Security): enable each item; CodeQL
analysis, Set up, Default. CodeQL runs on every pull request and push and
adds a `CodeQL` check; it is not added to the required checks.
