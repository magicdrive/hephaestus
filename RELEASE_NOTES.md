# Hephaestus v0.2.0

## Added

- **`hephaestus preflight --tag vX.Y.Z`**: read-only release safety checks to
  run before pushing a release tag, for any repository that releases with
  Hephaestus ([docs/preflight.md](https://github.com/magicdrive/hephaestus/blob/v0.2.0/docs/preflight.md)).
  - 25 checks with stable IDs over local git (`git.*`: work tree, branch,
    HEAD = remote branch), the tag (`tag.*`: format, absent locally and on
    the remote, order), the configuration (`config.*`: asset names unique,
    release settings, formula publication for the tag), GitHub
    (`github.*`: repository, no release or draft, CI on the exact commit,
    release workflow, environment deploying only from tag policies, secret
    names, branch and tag rulesets) and the Homebrew tap (`tap.*`:
    repository, formula directory, pull request branch and pull request).
  - Results `PASS`, `FAIL`, `WARN`, `SKIP`, `UNKNOWN` (could not be checked:
    permissions, rate limit, network) with evidence and remediation.
  - Human-readable output or `--format json` (schema version 1, JSON only on
    stdout); `--strict` treats `WARN` and `UNKNOWN` as failures; `--offline`
    runs only the local checks; `--skip` by check ID.
  - Exit codes: `0` pass, `1` fail (or strict `WARN`/`UNKNOWN`), `2` usage,
    `3` some checks unknown.
  - Optional `preflight:` section in `hephaestus.yaml` (`workflow`,
    `environment`, `secrets`, `tap_token_secret`, `branch`, `remote`,
    `skip`); command-line flags override it.
  - Read-only by construction: GitHub is reached only through `GET`
    requests, git only through an allowlist of read-only subcommands run with
    `--no-optional-locks` (no `fetch`, no index refresh), no repository code
    is run, only secret names are read, and tokens and credentials in remote
    URLs never appear in the output. Tests check every request method and
    snapshot a real repository and its remote.

## Improved

- Check runs are judged deterministically: every run is read
  (`filter=all`), runs are grouped by app and name and decided by the
  latest run (highest ID, else latest `started_at`), independently of the
  order the API returns them in; a newer pending run is never hidden behind
  an older success; required checks are matched to the app they name; runs
  that cannot be ordered and disagree are `UNKNOWN`.
- New read-only GitHub API calls (environments and deployment policies,
  secret names, rulesets, check runs, commit status, workflows) in the
  GitHub client.
- Shared logic: the archive names a configuration produces
  (`archive.ExpectedArchives`) and Homebrew's formula-directory rule
  (`tap.FormulaDirProblem`) are now used by the pipeline, `make publish`
  and the preflight alike.

## Compatibility

- The release pipeline (`build`, `archive`, `checksum`, `release`,
  `formula`, `tap`), the GitHub Action and GoReleaser compatibility behave
  as in v0.1.0.
- The `preflight:` section is optional; existing configurations load
  unchanged. A configuration that uses `preflight:` cannot be read by
  v0.1.0, which rejects unknown keys.

## Limitations

- The GitHub Action does not run the preflight yet (`command: preflight` is
  not available); run the CLI in a workflow step.
- `make publish-check`, which releases Hephaestus itself, still uses its
  own checks; it is not yet built on `hephaestus preflight`.
- With the default `GITHUB_TOKEN`, checks that read secret names,
  environments or rulesets may be `UNKNOWN`; use a token that can read them,
  or `--skip` what you accept not to verify.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v0.1.0...v0.2.0
