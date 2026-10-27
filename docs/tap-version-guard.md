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
go run github.com/magicdrive/hephaestus/cmd/tap-version-guard@<commit> [-base HEAD^1] [-head HEAD] [-allow-downgrade]
```

Exit status: `0` no downgrade, `1` a downgrade, `2` usage or git error. The
default range `HEAD^1..HEAD` is what a `pull_request` run checks out: the
merge of the pull request into the current base branch.

To adopt it, copy
[examples/workflows/tap-version-guard.yml](../examples/workflows/tap-version-guard.yml)
into the tap's `.github/workflows/` and, in a ruleset on the tap's default
branch:

- require the status check `version-guard`;
- require branches to be up to date before merging (strict), or use a merge
  queue. Without it the check of the older pull request may have run before
  the newer one was merged, and its green result is stale.

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
