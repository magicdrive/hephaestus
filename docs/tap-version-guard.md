# Formula downgrades through the merge order

In pull request mode every release opens its own formula pull request on the
tap. When the pull requests of two releases are open at once (v1.2.0 and
v1.3.0), merging them in the wrong order — v1.3.0 first, v1.2.0 last —
leaves the tap at v1.2.0. Each pull request was correct against the base it
was opened on; only the merge order makes it a downgrade.

Hephaestus guards against this in three layers. Only the last one, on the
tap, can enforce the order: Hephaestus never merges or closes tap pull
requests.

## 1. The tap stage (Hephaestus)

Before it proposes a formula, the tap stage compares versions by semantic
version precedence (`1.10.0` > `1.9.0`, `1.3.0` > `1.3.0-rc.1`, build
metadata ignored; never as strings) with:

| Compared with | Newer than this release | Older |
| --- | --- | --- |
| the formula on the base branch | skipped: nothing is written | proposed |
| the formula at the head of every other open pull request against the base branch | skipped: nothing is written | proposed; the older pull request is named in a warning and in the new pull request's body ("close it") |

A pull request proposing the same version, or a version that is not a
semantic version, is reported as a warning. Pull requests that do not change
the formula are ignored. `brews[].allow_downgrade: true` turns both skips
into warnings.

The scan runs again after the pull request is opened: two release runs racing
each other (both scanned before either opened) still report each other in
their warnings.

Every formula pull request body says that no older version may be merged
after it, and carries a marker `<!-- hephaestus-formula: <path> <version> -->`.

## 2. The merge order (reviewers)

Merge formula pull requests in version order, and close the older ones when a
newer one is open. The warnings of the release run and the pull request body
name them.

## 3. The tap's CI (proposal for the tap repository)

[`cmd/tap-version-guard`](../cmd/tap-version-guard/main.go) fails when a
change lowers the version of any formula (`*.rb` present on both sides),
and with `-strict` (what the example workflow runs, for a required check)
whenever it cannot show that the change is a plain upgrade:

```sh
go run github.com/magicdrive/hephaestus/cmd/tap-version-guard@<commit> [-base HEAD^1 [-each] | -against <tip>] [-head HEAD] [-strict] \
  [-rollback PATH=FROM->TO ... | -approvals <comments.json> -permissions <permissions.json> -approval-head <sha> [-approval-before <time>]] \
  [-summary <file>] [-allow-downgrade]
```

| Mode | Compares | For |
| --- | --- | --- |
| default (`-base HEAD^1`) | `base..head` | a `pull_request` run: the merge of the pull request into the base as it was when the run started |
| `-base <sha> -each` | every commit on the first-parent chain of `base..head` against its first parent | a merge queue entry (`merge_group.base_sha`), a push (`before..after`): a downgrade inside a group or a fast-forward of several merges fails even when the net change goes up |
| `-against <tip>` | the formulae `head` changed since its merge base with `tip`, at `tip` and at `head` | an unmerged pull request head against the base branch's current tip, with no merge commit: re-checking open pull requests after the base moved, and right before merging |

`-strict`, the rollback authorizations and `-allow-downgrade` apply to every
mode. Exit status: `0` nothing refused (warnings allowed), `1` a refused
change (an `::error` annotation per finding), `2` usage or git error, or an
approval file that cannot be read (the guard could not check: unknown or
missing revision, shallow history, a missing object, no git). A required
check fails on both `1` and `2`.

### What fails

| Change (per formula) | default | `-strict` | `-strict` + authorized rollback `P=A->B` (example workflow) | `-strict -allow-downgrade` (compatibility, not in the example) |
| --- | --- | --- | --- | --- |
| Upgrade (one formula or several) | GREEN | GREEN | RED if the change does not lower `P` from `A` to `B` (an unused authorization) | GREEN |
| New formula with a semantic version | GREEN | GREEN | GREEN (with `P` lowered) | GREEN |
| Same version, only other lines changed (`desc`, `test`, `caveats`, `install`) | GREEN | GREEN | GREEN (with `P` lowered) | GREEN |
| Downgrade (`2.0.0` → `1.5.0`, `1.10.0` → `1.9.0`) | RED | RED | warning for exactly `P` from `A` to `B`; RED for any other formula or version | warning |
| Prerelease downgrade (`1.3.0` → `1.3.0-rc.1`, `rc.10` → `rc.9`) | RED | RED | as a downgrade | warning |
| Same version, other artifacts (`url`, `sha256`, `mirror`, `head`, `root_url`, `resource`, `patch` and its `__END__` data, platforms swapped) | GREEN | RED | RED | warning |
| A url of the formula naming another version than the formula's (one platform left on the old release) | GREEN | RED | RED | warning |
| New side's version not a semantic version; versions that cannot be compared (including an old side without one) | warning / GREEN | RED | RED | warning |
| Formula removed | GREEN (skipped) | RED | RED (an approval never covers a removal) | warning |
| Layout changed (root `*.rb` ↔ `Formula/`): the formulae of the old layout are removed | GREEN | RED | RED | warning |
| No version on the new side (no `version` in the class body, no semantic version in its url), added formulae included | GREEN | RED | RED | **RED** |
| Formula that is not a regular file (symlink, submodule) | compared by its link text | RED | RED | **RED** |
| Path with control characters, `.` or `..` components | — | RED | RED | **RED** |
| `Formula` or `HomebrewFormula` that is not a directory (a symlink) | — | RED | RED | **RED** |
| `*.rb` that Homebrew does not load as a formula (`cmd/`, `Casks/`, `lib/`, root `*.rb` when `Formula/` exists) | compared as a formula | skipped (listed) | skipped | skipped |
| The guard cannot check (exit 2) | RED | RED | RED | RED |

**What an authorized rollback lowers: one downgrade, nothing else.** An
authorization names a formula path, the version it goes down from and the
version it goes down to (`-rollback P=A->B`, or an approval comment, see
[Intentional rollback](#intentional-rollback)). Only that exact downgrade
becomes a warning. Every other finding stays an error: another formula going
down, the same formula going to another version, a re-release, a removal, a
formula the guard cannot read. An authorization that matches no downgrade of
the change is an error too, so an approval always describes the change it lets
through. A refused run records no rollback.

**`-allow-downgrade` (compatibility).** It turns every finding about *which
version and artifacts the tap serves* into warnings (any downgrade, a
re-release, a removal, versions the guard cannot order), never one that means
the guard *cannot read the formula*. It says nothing about which formula or
version may go down, so the example workflow no longer uses it; it cannot be
combined with `-rollback` or `-approvals`.

**Why the same version with other artifacts fails.** A re-release under the
same version (a rebuilt archive, a moved asset) cannot be told from a
swapped artifact from the formula alone; users who installed the version
already have other bytes. Release a new version instead, or label the pull
request after verifying the new artifacts.

**Which files count.** `-strict` checks the formulae Homebrew loads from the
tap, as Homebrew finds them: every `*.rb` under `Formula/` (sharded
subdirectories included) when the tap has that directory, else under
`HomebrewFormula/`, else the `*.rb` files at the root of the tap (as in
magicdrive/homebrew-tap). Both sides' layouts count, so moving formulae
between layouts reports the old ones as removed. Contents are read from git
objects (`ls-tree`, `cat-file`), never from the work tree, so no path reaches
outside the repository; annotations escape paths and versions, so a crafted
file name cannot start a workflow command of its own. Without `-strict` the
guard keeps comparing every changed `*.rb` as before.

### The race of two release pull requests (TOCTOU)

The base has `1.0.0`; pull requests A (`2.0.0`) and B (`1.5.0`) are opened on
it. Covered by `TestTOCTOU` and `TestConcurrentPullRequests` on local
repositories:

| Check | Result |
| --- | --- |
| A and B, each against the old base (head, `-against`, the `pull_request` merge commit) | GREEN, GREEN |
| After A is merged: B `-against` the new tip | RED (`2.0.0` → `1.5.0`) |
| After A is merged: B merged into the new base, its conflict resolved for B, default mode (`HEAD^1..HEAD`) | RED |
| A and B landed by one push, `-base <before> -each` | RED (the net `1.0.0` → `1.5.0` alone would be GREEN) |
| Re-running B's old `pull_request` run (the merge commit of the old base) | GREEN: stale. Only a fresh merge sees the new base. The example workflow's `pull_request_target` run fetches the current base tip, so a re-run re-checks |

The stale green is what the merge has to refuse. `pr-gate` refuses it: it
requires the head to be 0 commits behind the base (`base.up_to_date`), so a
check that ran on an older base is never used
(`TestEvaluate_BehindBaseNeverPasses`). A merge in GitHub's UI is refused
only by the ruleset's **Require branches to be up to date before merging**
(strict): the tap of a personal account (magicdrive) cannot use a merge
queue.

### Why the pull request check alone is not enough

(About a `pull_request` check on the merge commit; the example workflow now
checks `-against` the base tip it fetches, but its result is just as old as
its run.) A `pull_request` check runs on the merge of the pull request into the base
**as it was then**. After a newer release is merged, the older pull
request's green check stays green; re-running it checks the same old merge
commit again (the run keeps its `GITHUB_SHA`). Two release pull requests
from one base change the same formula lines, so in practice the older one
conflicts and GitHub cannot merge it until someone resolves the conflict;
a resolution is a new commit, which runs the check again and fails. A
downgrade gets through only around that: a resolution that is not pushed
to the pull request (a manual merge pushed to the base), a merge commit
prepared before the newer release landed, formulae edited outside the
generated lines, an admin bypass, or a direct push.

### Options

| Option | Blocks a stale green check? | Cost | Where |
| --- | --- | --- | --- |
| Required status check `version-guard` | No, alone: a green result from an older base still counts | None | Ruleset |
| Required check + **require branches to be up to date** (strict) | Yes: the pull request must contain the current base, and updating it re-runs the check | Every merge needs an update of the other open pull requests | Ruleset |
| **Merge queue** + required check with `merge_group` | Yes: the check runs on the queue entry, built on the current base and the entries ahead of it (`-each` from `base_sha`) | Merges wait for the queue's run; available only to repositories owned by an organization (a tap under a personal account cannot use it) | Ruleset + `merge_group` trigger |
| Check after the merge (`push` to the base branch) | No, it detects: opens an issue with the restore steps | A downgrade may be visible until it is restored | Workflow (`after-merge` job) |
| Re-check open pull requests when the base moves | No, it detects: names the pull requests to close in the same issue | One run per push | Workflow (`after-merge` job, `-against`) |
| `workflow_dispatch` | No: a manual re-check of every open pull request | Manual | Workflow (`after-merge` job) |
| Update the branch (GitHub's "Update branch") | Only together with strict: it creates the fresh merge the check runs on | Manual per pull request | — |
| A GitHub App that sets a required status on every open pull request when the base moves | Yes, if its status is required | An App to write, host and trust with `statuses: write` | Outside this repository |
| Reviewers merge in version order (Hephaestus names older pull requests) | Only as reliable as people | None | Pull request body, release run warnings |

The recommended setting is a merge queue, or strict status checks when the
tap cannot use one (a personal account's tap); the `after-merge` job is a detector for what gets
around either (bypass, direct push), not a replacement.

### Adopting it

Copy [examples/workflows/tap-version-guard.yml](../examples/workflows/tap-version-guard.yml)
into the tap's `.github/workflows/`, replace `<full-commit-sha>` with the
Hephaestus commit you reviewed (one with `-approvals`), and set the ruleset of
the tap's default branch after the bootstrap below. The workflow compiles the
guard with an exact Go (`go-version`, no cache) and `GOTOOLCHAIN=local`: when
a newer pinned commit needs a newer Go, the build fails instead of fetching
another toolchain; raise `go-version` together with the commit.

What the example workflow does, and how each part fails:

- **version-guard** (`pull_request_target`; `merge_group`): the required
  check. `pull_request_target` runs the workflow file of the **default
  branch**, not the pull request's (`pull_request` runs the one in the merge
  commit, so a pull request could edit the check, the label test or the
  approval test away). The job checks out the default branch, fetches the
  base branch tip and `refs/pull/<n>/head` into refs of their own (never into
  the work tree), verifies the fetched head is the event's head, and runs
  `-strict -against <base tip> -head <head SHA>`. It runs no code of the pull
  request (it reads formulae as git objects) and uses no secret; its token
  has `contents: read` and `pull-requests: read`. `opened`, `synchronize`,
  `reopened`, `edited` (base changed), `labeled` and `unlabeled` run it
  again. On `merge_group` it checks every step of the queue entry (`-each`)
  and never takes an approval.
- **version-guard-pending / version-guard-verdict**: post the commit status
  `version-guard` on the pull request head: `pending` when the run starts,
  then `success` only when the version-guard job succeeded (failure,
  skipped or anything else: `failure`). They hold only `statuses: write`,
  run no action, no git and no guard, and read nothing of the pull request
  but its head SHA. Why: a pull request (a fork's included) can add a
  workflow of its own on `pull_request` with a job named `version-guard`;
  that job's check run carries the same name. GitHub requires a check and a
  commit status of the same name **both** to pass, and the fork's read-only
  token cannot post the status, so the spoofed check cannot stand in for
  this one. Runs of one pull request are serialized (`concurrency`, newest
  event wins); a cancelled run posts nothing and leaves `pending`.
- **after-merge** (`push` to the default branch, `workflow_dispatch`): checks
  each first-parent commit of the push with the rollback approvals of its own
  pull request (the label, an admin's approval of its head posted **before the
  merge**; a failed read counts as no approval), then re-checks every open
  pull request `-against` the new tip, with its approvals. A pull request
  that fails against its own merge base is not reported (its own required
  check blocks it). Findings go to the issue; anything it could not check
  (exit 2, a pull request it cannot fetch or list) fails the job and opens no
  issue. Authorized rollbacks that landed are recorded in the step summary.
- **after-merge-issue**: the only job with `issues: write`; it runs no
  action, no git and no guard, only `gh issue` with the report.
- Every script runs `bash -euo pipefail`, every event value reaches it
  through `env` (and is validated: numbers, 40-digit SHAs, branch names),
  actions are pinned to commit SHAs, the checkout keeps no credentials, and
  the workflow's default permissions are `{}`. The ruleset is the tap owner's
  decision; nothing in Hephaestus changes it.

**Bootstrap (the first install).** `pull_request_target` runs the workflow of
the default branch, so the pull request that adds the workflow is not checked
by it (and a `version-guard` required before it exists would block every pull
request, that one included). Install in this order:

1. Merge the pull request that adds `formula-version-guard.yml` (and, if you
   like, a lint-only workflow on `pull_request` with another check name, such
   as `workflow-lint` running actionlint; never name it `version-guard`, and
   never make it required: a pull request controls what it runs) while
   `version-guard` is not required yet. It changes no formula.
2. Open any pull request (or push to an open one) and confirm that the
   `version-guard` check run and the `version-guard` commit status appear on
   its head.
3. Then make `version-guard` required (below).

If a ruleset already requires `version-guard`, the bootstrap pull request
needs that rule disabled for the one merge (see
[release-protection.md](release-protection.md)); never delete the ruleset.

**Before 2 December 2026** (the date GitHub's changelog of 2026-09-17 gives):
GitHub enforces a default Actions event policy that **blocks
`pull_request_target` in public repositories** without an applicable policy.
Allow `pull_request_target` for the tap in an Actions event policy (Settings,
Actions) or the check never runs: every pull request then waits for
`version-guard` forever (fail closed). Also set "Require approval for all
external contributors" for fork pull request workflows (Settings, Actions):
the commit status covers a spoofed check either way, but fewer fork
workflows run.

**In the GitHub UI** (tap repository, Settings, Rules, Rulesets, the ruleset
targeting the default branch, or New branch ruleset):

1. Enforcement status: Active. Target branches: Include default branch.
2. Enable **Require status checks to pass**; Add checks: `version-guard`
   (source: GitHub Actions; the check run and the commit status of that name
   both come from the workflow's token); enable **Require branches to be up
   to date before merging**.
3. Or, with a merge queue: enable **Require merge queue** (merge method
   Merge, build concurrency as needed, "Only merge non-failing pull
   requests"); `version-guard` stays a required check, and the workflow's
   `merge_group` trigger runs it on each entry.
4. Keep the bypass list empty (or know that a bypass is what the
   `after-merge` job then reports).
5. Create the label `formula-rollback` (needed for rollbacks; only people
   with triage access or more can set labels).

**With the API** (`gh`, an admin token of the tap; `TAP=owner/homebrew-tap`):

```sh
# Find the ruleset of the default branch (or create one below).
gh api "repos/$TAP/rulesets" --jq '.[] | {id, name, target, enforcement}'

# Create it: required check, strict, no bypass.
gh api -X POST "repos/$TAP/rulesets" --input - <<'JSON'
{
  "name": "default branch",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "bypass_actors": [],
  "rules": [
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": true,
      "required_status_checks": [{"context": "version-guard", "integration_id": 15368}]
    }}
  ]
}
JSON
```

`integration_id` 15368 is GitHub Actions; omit it to accept the check from
any source. To use a merge queue instead of (or with) strict checks, add
this rule to `rules` (and update an existing ruleset with
`gh api -X PUT "repos/$TAP/rulesets/<id>" --input <file>`, sending the
whole ruleset):

```json
{"type": "merge_queue", "parameters": {
  "merge_method": "MERGE", "grouping_strategy": "ALLGREEN",
  "max_entries_to_build": 5, "min_entries_to_merge": 1, "max_entries_to_merge": 5,
  "min_entries_to_merge_wait_minutes": 0, "check_response_timeout_minutes": 60
}}
```

Check the result: `gh api "repos/$TAP/rules/branches/main"` lists the
active rules of the branch.

### Re-checking right before a merge

Hephaestus scans the open pull requests when it proposes a formula and again
after opening its pull request, but it never merges. Whoever merges can
re-check a formula pull request against the current base without a merge
commit:

```sh
git -C <tap> fetch origin main "pull/<number>/head"
go run github.com/magicdrive/hephaestus/cmd/tap-version-guard@<commit> \
  -C <tap> -against origin/main -head FETCH_HEAD
```

or run the workflow manually (`gh workflow run "Formula version guard" -R
$TAP`), whose `after-merge` job re-checks every open pull request against
the default branch and opens an issue for those that would lower a formula.

## Intentional rollback

A rollback is never inferred, and a label alone never allows one.

- **From a release run:** set `brews[].allow_downgrade: true` for the run that
  publishes the older version (the downgrade is reported as a warning). The
  tap's check still requires the approval below.
- **On the tap (example workflow):**
  1. Open the pull request that lowers the formula (or use the one the
     release run opened). Note its head SHA (`gh pr view <n> --json
     headRefOid`).
  2. As an **admin of the tap** (on a personal account's repository: its
     owner), post a comment on the pull request, exactly:

     ```text
     /formula-rollback
     head: <the 40-digit head SHA>
     formula: <path> <from> -> <to>
     reason: <why, e.g. 1.3.0 corrupts the cache (#12)>
     ```

     One `formula:` line per formula that goes down (`formula: tool.rb 1.3.0
     -> 1.2.0`); blank lines are allowed, nothing else.
  3. Then add the label `formula-rollback`. Adding it runs the check again;
     posting a comment does not. (Comment first, label second; or remove and
     add the label again.)
  4. The check passes when the label is there and an approval comment of an
     admin names exactly this head, this formula and these versions. The run's
     summary records the comment URL, the approver, the head, the formula, the
     versions and the reason.
  5. After the merge, remove the label. The `after-merge` job checks the
     merge with the same rules (the approval must be older than the merge).

What the check requires of an approval comment, and why:

| Requirement | Why |
| --- | --- |
| The pull request has the label `formula-rollback` | an explicit, visible switch; removing it fails the check again (`unlabeled` runs it) |
| The comment's author has `permission` **and** `role_name` `admin` on the tap (`GET repos/{repo}/collaborators/{login}/permission`) | `author_association`, the pull request's author and labels can be had without admin: a collaborator with write is `COLLABORATOR`, labels need only triage |
| `head:` is the pull request's head SHA | a push after the approval (`synchronize`) fails the check: the approval covered other content |
| `formula:` names the path, the version on the base branch and the version of the head | only that downgrade is lowered; another formula, another version or a base that moved since (a newer release merged) fails |
| `updated_at` equals `created_at` | an edited comment (people with write access can edit comments) is not the approval that was given; post a new one |
| Exact format: first line `/formula-rollback`, one `head:`, one or more `formula:`, one non-empty `reason:`, nothing else | a comment that only mentions the command, or carries other text, is not an approval |
| Every API read succeeds | a failed read of the pull request, its comments or a permission fails the check (fail closed) |

A comment that fails any of these is listed as a notice and ignored: a
comment of someone else neither blocks the owner's approval nor stands in for
it. Bots' comments are not looked up. A removal, a re-release under the same
version and a formula the guard cannot read are never covered by an approval;
they stay the owner's decision outside this check (for example a ruleset
bypass, which the `after-merge` job then reports).

### Rollback authorization: threat model

| Threat | Countermeasure | Remaining risk |
| --- | --- | --- |
| A pull request edits the guard workflow (or the label/approval test) so it always passes | `pull_request_target` runs the default branch's workflow; the pull request is fetched as objects only | a workflow change merged by someone with write access takes effect after the merge: review workflow changes |
| A pull request adds its own workflow with a job named `version-guard` | the commit status `version-guard` must pass as well (a check and a status of the same name are both required); a fork's token cannot post statuses | a collaborator with write access **and** the `workflow` scope can post statuses and add workflows: such a person is trusted like the owner |
| A fork's code runs with the base repository's token | nothing of the pull request is executed or checked out; the token is read-only; no secret is used | the guard's own parser reads formulae as text (Go, no evaluation) |
| Someone without admin "approves" (a writer, a triager, a fork's author with `author_association` spoofing) | admin permission and role from the permission API; `author_association` is not read | an admin account compromise |
| The label alone | the label is necessary, never sufficient | — |
| The head changes after the approval | `head:` must equal the head; `synchronize` re-runs the check | — |
| The label is removed | `unlabeled` re-runs the check; the current labels are read from the API | — |
| The approval comment is edited or deleted after the check passed | edited comments are rejected on every later run, and `after-merge` re-validates after the merge | no event runs the check on an edit or a deletion: the green result of the original approval stays until the next event; `after-merge` reports it after the merge |
| An approval for one formula or version lets another through | exact path/from/to match; any other finding stays an error; unused authorizations fail | — |
| The base moves (a newer release) after the approval | `from` must equal the base tip's version; strict up-to-date branches | — |
| An older run (of a removed label) finishes last and posts success | runs of one pull request are serialized (`concurrency`), a cancelled run posts nothing | — |
| The API fails or GitHub blocks the trigger | every failed read fails the check; a check that never runs leaves the required context missing | GitHub's default policy blocking `pull_request_target` (public repositories, enforced 2 December 2026) must be lifted by the owner |
| A merge queue entry | no approval passes the queue (`merge_group` runs the entry's workflow) | merge a rollback without the queue |

## Limits

- A formula renamed in the same change is compared as removed and added.
- Without `-strict`, formulae whose versions are not semantic versions are
  reported, not blocked, and a same-version artifact change, a removal or a
  formula without a version passes.
- The guard reads formulae as text in the layout of generated and
  brew-style formulae (indentation, `do` blocks); it does not evaluate Ruby.
  A version bump that keeps the old artifacts, or an artifact of the right
  version name with other bytes, is not detected (the release's checksums
  and attestations are).
- Hephaestus reads at most 100 open pull requests against the base branch;
  beyond that it warns that the rest were not checked.
