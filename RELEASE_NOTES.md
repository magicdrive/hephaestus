# Hephaestus v1.3.0-rc.1

Release candidate of v1.3.0. `hephaestus publish` no longer assumes that
releases are cut from `main`: it tells the repository's **default branch**
from the **release branch**, and checks everything that guards a release on
the release branch.

## Release branch

```sh
hephaestus publish check v1.2.3                          # the default branch: main, master, develop...
hephaestus publish check -branch release/stable v1.2.3   # a named release branch
```

```yaml
preflight:
  branch: release/stable   # read by hephaestus publish and hephaestus preflight
```

The release branch is `-branch`, else `preflight.branch`, else the release
repository's default branch on GitHub. A repository whose default branch is
`develop` can release from `release/stable`; one on `master` needs no
setting. The preflight prints both:

```
  [ok  ] default branch               develop (GitHub, origin/HEAD)
  [ok  ] release branch               release/stable (preflight.branch) at 1a2b3c4d5e6f on GitHub
```

Everything is checked on the release branch, never on the default branch on
its behalf:

- it is a branch name that exists on GitHub: not a tag, a commit SHA, a full
  ref or a refspec;
- the clone is on it, clean, with `HEAD` = `origin/<branch>` = its head on
  GitHub;
- CI succeeded on it, for exactly that commit: a green default branch does
  not count;
- an active ruleset covering it requires `ci-gate`;
- right before the tag push it is fetched again and its head on GitHub read
  again: a branch that moved stops publish before anything is pushed;
- `hephaestus preflight --strict`, which publish also runs, checks the same
  branch.

When a branch cannot be read (no network, no access), or a local
`refs/remotes/origin/HEAD` names another branch than the default branch,
nothing further is checked and nothing is tagged.

See [docs/publish.md](https://github.com/magicdrive/hephaestus/blob/v1.3.0-rc.1/docs/publish.md#release-branch).

## Fixes

- A ruleset covering `~DEFAULT_BRANCH` counted as protecting whichever branch
  was checked. It now counts only for the repository's default branch, in
  `hephaestus publish` and in `hephaestus preflight` (`github.ruleset.branch`
  and the required checks of `github.ci`).

## Compatibility

- **Unchanged:** configuration (`preflight.branch` is the existing key), the
  Action, `publish plan` and `publish status`, JSON fields and check IDs.
- **New:** the `-branch` flag of `hephaestus publish`, and its `default
  branch` and `release branch` checks. A project on `main` gets the same
  checks as before plus these two; the ruleset check keeps its name
  (`main ruleset`).
- **Stricter:** `hephaestus preflight` with a `preflight.branch` other than the
  default branch now warns (`github.ruleset.branch`) when only the default
  branch's ruleset covers it.

## Known limitations

- `hephaestus check` shows the tap pull request branch as a template;
  `hephaestus preflight --tag` renders it.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.2.0...v1.3.0-rc.1
