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
change lowers the version of any formula (`*.rb` present on both sides):

```sh
go run github.com/magicdrive/hephaestus/cmd/tap-version-guard@<commit> [-base HEAD^1 [-each] | -against <tip>] [-head HEAD] [-allow-downgrade]
```

| Mode | Compares | For |
| --- | --- | --- |
| default (`-base HEAD^1`) | `base..head` | a `pull_request` run: the merge of the pull request into the base as it was when the run started |
| `-base <sha> -each` | every commit on the first-parent chain of `base..head` against its first parent | a merge queue entry (`merge_group.base_sha`), a push (`before..after`): a downgrade inside a group or a fast-forward of several merges fails even when the net change goes up |
| `-against <tip>` | the formulae `head` changed since its merge base with `tip`, at `tip` and at `head` | an unmerged pull request head against the base branch's current tip, with no merge commit: re-checking open pull requests after the base moved, and right before merging |

Exit status: `0` no downgrade, `1` a downgrade, `2` usage or git error.

### Why the pull request check alone is not enough

A `pull_request` check runs on the merge of the pull request into the base
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
Hephaestus commit you reviewed (one with `-against` and `-each`), and set
the ruleset of the tap's default branch. The ruleset is the tap owner's
decision; nothing in Hephaestus changes it.

**In the GitHub UI** (tap repository, Settings, Rules, Rulesets, the ruleset
targeting the default branch, or New branch ruleset):

1. Enforcement status: Active. Target branches: Include default branch.
2. Enable **Require status checks to pass**; Add checks: `version-guard`
   (source: GitHub Actions); enable **Require branches to be up to date
   before merging**.
3. Or, with a merge queue: enable **Require merge queue** (merge method
   Merge, build concurrency as needed, "Only merge non-failing pull
   requests"); `version-guard` stays a required check, and the workflow's
   `merge_group` trigger runs it on each entry.
4. Keep the bypass list empty (or know that a bypass is what the
   `after-merge` job then reports).
5. Optionally create the label `formula-rollback`.

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

A rollback is never inferred; it is always explicit:

- **From a release run:** set `brews[].allow_downgrade: true` for the run that
  publishes the older version (the downgrade is reported as a warning).
- **On the tap:** label the pull request `formula-rollback` (with the example
  workflow), which runs the guard with `-allow-downgrade`. A manual rollback
  is an ordinary pull request with that label.

Remove the flag or label afterwards: it disables the guard for that change.

## Limits

- A formula renamed in the same change is compared as removed and added.
- Formulae whose versions are not semantic versions are reported, not
  blocked.
- Hephaestus reads at most 100 open pull requests against the base branch;
  beyond that it warns that the rest were not checked.
