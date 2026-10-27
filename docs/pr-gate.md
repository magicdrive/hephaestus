# Merge gate (`cmd/pr-gate`)

`cmd/pr-gate` decides whether a pull request may be merged and, only when
asked with `-merge`, merges it pinned to the commit that was reviewed. It is
meant for automation (and people) that merge pull requests after CI: every
condition fails closed, so an API error, an unknown state or a missing check
stops the merge instead of letting it through.

It is a separate command, not a `hephaestus` subcommand: Hephaestus publishes
releases, and merging pull requests is not part of that pipeline. It reuses
Hephaestus's GitHub client (`internal/github`: retries with backoff for
reads, timeouts, size limits, token never sent to another origin).

```bash
GH_TOKEN=$(gh auth token) go run github.com/magicdrive/hephaestus/cmd/pr-gate@<commit> \
  -repo OWNER/REPO -pr 42 \
  -head <full head SHA> -base main -base-sha <full base SHA> \
  [-require NAME]... [-ci-gate ci-gate] [-json] [-merge [-method merge|squash]]
```

Without `-merge` it is a dry run: only GET requests, a report of every
condition, exit 0 only when all pass. `-merge` evaluates everything again,
and merges only if all conditions still hold.

`-head` and `-base-sha` are full 40-character SHAs: the commit that was
reviewed and the base branch commit it was tested against. Abbreviated SHAs
and branch names are refused.

## Conditions

| Finding | Passes only when |
| --- | --- |
| `pr.read` | the pull request could be read |
| `pr.state` | it is open and not merged |
| `pr.draft` | it is not a draft |
| `pr.head` | its head is exactly `-head` |
| `pr.base` | its base branch is `-base` |
| `pr.mergeable` | GitHub reports `mergeable: true` and `mergeable_state: clean`. While GitHub is still computing it (`null` / `unknown`) the pull request is read again, at most `-mergeable-reads` times, `-mergeable-wait` apart; still unknown is an error |
| `base.sha` | the base branch is exactly at `-base-sha` |
| `base.up_to_date` | comparing `-base-sha...-head` (by commit, not branch name): `ahead`, behind by 0 |
| `required.read` | the active branch rules (`GET rules/branches/{branch}`, repository and organization rulesets) and the classic branch protection could be read, and the set of required checks is not empty. A branch that requires no checks needs `-require` |
| `checks.read` | the check runs (`filter=all`, every page), the commit statuses (all of them) and the workflow runs of the head could be read |
| `checks.present` | there is at least one check run or status |
| `checks.completed` | every check run, status and workflow run has completed |
| `checks.no_failure` | no latest result is `failure`, `cancelled`, `timed_out`, `action_required`, `stale`, `startup_failure`, an error status or a value the gate does not know. `skipped` and `neutral` are accepted only for checks nobody requires |
| `checks.required` | every required check (rulesets, branch protection, `-require`) has a latest result, from the required app when one is named, and every such result is exactly `success` |
| `checks.ci_gate` | the `-ci-gate` check (default `ci-gate`) succeeded, reported by `-ci-gate-app` (default 15368, GitHub Actions; `0` accepts any source, including a commit status) |

Re-runs: check runs are grouped by name and app, and each group is decided
by its latest run (the highest ID; a re-run attempt gets a new, higher ID).
An older success never hides a newer pending or failed attempt. When two
runs cannot be ordered and their outcomes differ, the check fails. Workflow
runs are grouped by workflow and event the same way, so a run that failed
before starting any job (an invalid workflow file) fails the gate even though
it produced no check run.

## Merging

With `-merge`, after every condition passed:

1. the pull request and the base branch are read once more (head, draft,
   state, base branch, base commit);
2. one `PUT /repos/{owner}/{repo}/pulls/{n}/merge` is sent with
   `sha` = `-head`, so GitHub refuses (409) if the head moved. It is never
   retried: a lost response may hide a merge;
3. the pull request is read again, whatever the response, and the merge
   commit's parents are compared with `-base-sha` and `-head` (for
   `squash`, the first parent with `-base-sha`).

The merge API cannot pin the base branch. A base that moves in the moment
between step 1 and step 2 is detected by step 3 and reported (exit 4); it
cannot be prevented by the gate. Requiring branches to be up to date in the
ruleset (`strict_required_status_checks_policy`) makes GitHub refuse such a
merge as well. Two `pr-gate -merge` runs on one machine for the same base
branch are serialized with a lock file in the temporary directory.

`rebase` is not offered: its result cannot be verified by the parents.

## Exit status

| Code | Meaning |
| --- | --- |
| 0 | every condition passed; with `-merge`, merged and the merge commit verified |
| 1 | a condition failed, or GitHub refused the merge; nothing was merged |
| 2 | usage error (nothing was read) |
| 3 | a condition could not be decided (API error, rate limit, unknown state), or the lock is held; nothing was merged |
| 4 | the merge request was sent and the result needs a person: merged but the parents do not match, or whether it merged is unknown |

## Token

Read from `GH_TOKEN`, else `GITHUB_TOKEN`. The dry run needs read access to
the pull request, checks, actions, branch rules and contents; `-merge` needs
`contents: write` and `pull_requests: write`. A token whose role can bypass
the rulesets is still held to every condition here.
