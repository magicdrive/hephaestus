# GitHub Action

`action.yml` at the repository root is a composite action. It installs the
Hephaestus release binary that belongs to the commit you reference, then runs
the `hephaestus` command in your workspace: the local stages (`build`,
`archive`, `checksum`) in a step without tokens, then the publishing stages
(`release`, `formula`, `tap`) in a step with them. It does not check out
your repository and does not install your build toolchain: add
`actions/checkout` and, for example, `actions/setup-go` before it.

```yaml
- uses: magicdrive/hephaestus@<full commit SHA of the release tag> # vX.Y.Z
  with:
    command: run
    stages: build,archive,checksum
```

### Pinning and the release binary

Pin the action to the full commit SHA that a release tag points to (for
v1.2.0: `git rev-parse v1.2.0^{commit}`), or reference it by that tag. The
action does not compile anything. Its first step
([action/install-hephaestus.sh](../action/install-hephaestus.sh)) runs before
any of your build code, and does the following:

1. Reads the release named in `action/VERSION` of the pinned commit. The
   release workflow refuses to publish a tag that this file does not name,
   so a release commit always names its own release.
2. Downloads `hephaestus_<version>_<os>_<arch>.tar.gz` (Windows: `.zip`) and
   `hephaestus_checksums.txt`, from
   `https://github.com/magicdrive/hephaestus/releases/download/<tag>/` only
   (https, redirects https only).
3. Checks the archive's SHA-256 against the checksum file.
4. Verifies the archive's provenance attestation with `gh attestation
   verify`. The attestation must have been signed by
   `magicdrive/hephaestus/.github/workflows/release.yml` on a GitHub-hosted
   runner, for `refs/tags/<tag>` and, when pinned by SHA, for exactly that
   commit (`--source-digest`). This binds the binary to the commit you
   pinned.
5. Extracts only the regular file `hephaestus[.exe]` at the archive root:
   links, directories, nested paths and duplicates are rejected, and no
   other member is written. It then checks that the binary reports
   `hephaestus <tag>`.

Any failure stops the action. There is no fallback, and no other version is
tried. A branch reference (`@main`) or the tag of another release fails,
because the action could not tell which binary belongs to it.

Requirements: a GitHub-hosted runner, or another runner with `bash`,
`curl`, `tar` and `gh`, on Linux, macOS or Windows, x64 or ARM64. The step
uses the job's `github.token` for the read-only attestation lookup. No step
that runs your code sees that token, and it does not depend on the
`github-token` input.

### The action does not change your toolchain

No Go (or any other toolchain) is downloaded or set up, and nothing is
written to `$GITHUB_PATH` or `$GITHUB_ENV`. Your build commands run with the
`PATH`, `go` and `GOTOOLCHAIN` that your own steps set up, before and after
the action. A runner without Go can run the action for a project that is
not written in Go. CI compares the environment before and after the action
on six platforms (`action-isolation`, `action-install`).

Earlier versions behaved differently:

- **v1.1.0** ran `actions/setup-go`. That step put its Go first on `PATH` and
  exported `GOTOOLCHAIN=local` for the rest of the job.
- **The unreleased v1.1.1 fixes** compiled Hephaestus with a private Go. They
  ship in v1.2.0.

From v1.2.0, a project that relied on the action's Go must set up its own
Go, as the examples always did.

### Hephaestus's own checkout

When a workflow runs the action from its own checkout of this repository
(`uses: ./`), the version in `action/VERSION` is usually not released yet.
Only in that case does the action use the binary named by the
`HEPHAESTUS_ACTION_BINARY` environment variable. Hephaestus's CI sets it to
a binary built from the same commit (the `action-binary` job). Every other
caller ignores the variable.

The action runs on Linux, macOS and Windows runners (CI runs `action-smoke`
and `action-boundary` on all three). Its steps use `bash`; on Windows that is
Git Bash, paths are passed to Go as `D:/a/_temp`-style paths and the binary
is `hephaestus.exe`. Build commands named `bash` are found by `PATH`; on
Windows put Git Bash first (`C:\Program Files\Git\bin`) if a WSL `bash`
could shadow it.

## Inputs

| Input | Default | Meaning |
| --- | --- | --- |
| `command` | `run` | `run`, `check`, `build`, `archive`, `checksum`, `release`, `formula`, `tap`, `preflight` or `plan` ([release plan](#release-plan)). Any other value fails the step. |
| `stages` | `""` | With `command: run`: comma-separated stages (`build,archive,checksum,release,formula,tap`) or `all`. Empty runs every stage the configuration declares. Ignored by other commands. |
| `config` | `""` | Configuration file, relative to `working-directory`. Empty: `hephaestus.yaml` (or `.yml`, `.hephaestus.yaml`, `.hephaestus.yml`). |
| `tag` | `""` | Release tag. Empty: the pushed tag (`github.ref_name` when the workflow runs for a tag). |
| `strict` | `"false"` | With `command: preflight`: `"true"` treats `WARN` and `UNKNOWN` checks as failures. |
| `offline` | `"false"` | With `command: preflight`: `"true"` runs only the checks that need no network. |
| `snapshot` | `"false"` | `"true"` builds without a tag (version `0.0.0-SNAPSHOT`). Configured release, formula and tap stages (they need a tag) are skipped; naming them in `stages` is an error. |
| `dry-run` | `"false"` | `"true"` plans release and tap writes without performing them (reads still use the tokens when given). |
| `fresh` | `"false"` | `"true"` ignores an existing manifest instead of continuing from it, and deletes the outputs it records that this run writes again ([`--fresh`](cli.md)). |
| `working-directory` | `.` | Directory to run in (the project root when the configuration is there). |
| `github-token` | `${{ github.token }}` | Token for the release repository, exported as `GITHUB_TOKEN`. Needs `contents: write` to publish. Set only in the publishing step (`release`, `formula`, `tap`, and the publish phase of `run`). |
| `tap-token` | `""` | Token for the Homebrew tap, exported as `HOMEBREW_TAP_GITHUB_TOKEN`. `contents: write` (and `pull-requests: write` for pull request mode) on the tap. `GITHUB_TOKEN` cannot write to another repository. Set only in the publishing step, like `github-token`. |

`tag`, `snapshot`, `dry-run`, `fresh` and `stages` are not passed to
`command: check`. Inputs reach the script through environment variables, never
by `${{ }}` interpolation into shell code, and `command` is checked against
the list above.

## Outputs

| Output | Value |
| --- | --- |
| `status` | `published`, `no-op`, `dry-run` or `failed`. `no-op` also covers runs that write nothing to GitHub (for example build, archive and checksum only). |
| `no-op` | `"true"` when the run succeeded and changed nothing on GitHub. |
| `version` | The version being released (`1.2.3`, or `0.0.0-SNAPSHOT`). |
| `artifacts` | JSON list of archives and checksum files: `[{"name", "path", "sha256", "url"}]`. `url` is empty until the release is published. |
| `release-url` | URL of the GitHub Release (set when the release stage ran). |
| `release-id` | Numeric id of the GitHub Release. |
| `uploaded-assets` | JSON list of asset names uploaded or replaced by this run. |
| `formula-paths` | Newline-separated, project-relative paths of generated formulae. |
| `formula-commit-sha` | Newline-separated tap commit SHAs written by this run. |
| `pull-request-url` | Newline-separated tap pull request URLs (opened or reused). |
| `preflight-status` | With `command: preflight`: `pass`, `warn`, `unknown` or `fail`. |
| `preflight-exit-code` | With `command: preflight`: `0` pass, `1` fail (or strict `WARN`/`UNKNOWN`), `2` usage, `3` some checks unknown. The step fails on any non-zero code. |
| `preflight-report` | With `command: preflight`: path of the JSON report ([schema](preflight.md#json-report)). |
| `prerelease` | With `command: plan`: `"true"` when the tag is released as a prerelease. |
| `tap` | With `command: plan`: `publish` when the release updates the Homebrew tap, `skip` otherwise. |

The step also writes a Markdown summary (stages, artifacts with SHA-256,
release assets and their status, formulae, tap results, warnings). Known
token values are replaced by `***` in errors, the summary and outputs, and
tokens are registered with `::add-mask::` for the log.

## Permissions

| Stage | `GITHUB_TOKEN` permissions | Tap token |
| --- | --- | --- |
| `check`, `build`, `archive`, `checksum`, `formula` | `contents: read` (checkout only) | — |
| `release` | `contents: write` | — |
| `tap` | — | `contents: write` on the tap; `pull-requests: write` in pull request mode |

Declare `permissions: contents: read` at the top of the workflow and grant
`contents: write` only to the job that publishes. `id-token: write` and
`attestations: write` are needed only if you add
`actions/attest-build-provenance`.

## Token design

- **Two tokens, two scopes.** The workflow's `GITHUB_TOKEN` publishes the
  release in the same repository. It cannot write to another repository, so
  the tap needs its own token. Keep them separate; never use one personal
  access token for both.
- **Tap token: a GitHub App installation token or a fine-grained personal
  access token**, limited to the tap repository with `contents: write` and
  `pull-requests: write`. A GitHub App token also makes the tap commits
  signed by GitHub. Avoid classic personal access tokens.
- **Store it in an environment** (for example `release`) with protection
  rules (required reviewers, tag-only deployment), not as a plain
  repository secret.
- **Give each step only what it needs.** The action runs `check`, `build`,
  `archive`, `checksum` and the local phase of `run` (`--phase local`) in a
  step whose environment has no token at all, and `release`, `formula`,
  `tap` and the publish phase of `run` (`--phase publish`) in a separate
  step that gets them. Steps inherit the job's and the workflow's `env`, so
  the local step blanks `GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_PAT`,
  `HOMEBREW_TAP_GITHUB_TOKEN`, the GitHub Enterprise token names and the
  runner's OIDC request token; **pass tokens through the action's inputs,
  never through job or workflow `env`**, because a token under any other
  name would still be inherited by the local step. CI checks this boundary
  on real Linux, macOS and Windows runners (`action-boundary` in `ci.yml`:
  `/proc` on Linux, `ps -E` on macOS, the build's own environment on
  Windows). Hephaestus also removes the tokens, the variables
  `repository.token` reads, the runner's `ACTIONS_*` tokens and
  credential-looking variables from the environment of build commands unless
  a build names them in its `env`.
- **That is not isolation.** Build code runs as the same user as everything
  else in the job. It can read the environment of the processes around it
  (`ps -E` on macOS, `/proc/PID/environ` of the step's shell on Linux;
  Hephaestus itself is non-dumpable on Linux), and a process left running by
  a build step can wait for the publishing step and read its environment.
  When you publish, **build and publish in separate jobs** (see below).
- **Templates do not expose tokens.** `{{ .Env.NAME }}` hides the token
  variables, the variables `repository.token` reads, and variables whose
  name contains `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `PASSPHRASE`,
  `CREDENTIAL`, `PRIVATE_KEY`, `API_KEY`, `APIKEY` or `ACCESS_KEY`, or has a
  `_`-separated segment `PAT`, `AUTH`, `KEY`, `PASS` or `PWD` (except the
  shell's `PWD` and `OLDPWD`), unless they
  are declared in the configuration's top-level `env`. A rendered value that
  contains a token is an error. Only `builds[].env` values and
  `brews[].repository.token` see the complete environment. Name detection
  is a heuristic: keep credentials under recognizable names.
- Set `github-token: ""` on steps that do not publish (pull request CI,
  snapshot builds).

## Release plan

`command: plan` prints what a release workflow does for a tag
(`hephaestus publish plan`, [publish.md](publish.md)). It sets the outputs
`prerelease` and `tap`, so the steps that use the tap token can be skipped
for a prerelease and never receive the token. The tag is the `tag` input,
or the pushed tag. The plan reads only the configuration: it needs no token
and runs no project code. A prerelease whose plan would update the tap fails
the step. `hephaestus publish` expects a release job built this way:

```yaml
      - name: Plan the release
        id: plan
        uses: magicdrive/hephaestus@<full commit SHA of a release tag> # vX.Y.Z
        with:
          command: plan
      - name: Generate the formula and open a tap pull request
        if: steps.plan.outputs.tap == 'publish'
        uses: magicdrive/hephaestus@<full commit SHA of a release tag> # vX.Y.Z
        with:
          command: run
          stages: formula,tap
          github-token: ""
          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

## Release preflight

`command: preflight` runs [`hephaestus preflight`](preflight.md) — read-only
release safety checks — before you push a tag, e.g. in a manually dispatched
"prepare release" workflow:

```yaml
permissions:
  contents: read
  actions: read
jobs:
  preflight:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: magicdrive/hephaestus@<full-commit-sha>
        id: preflight
        with:
          command: preflight
          tag: ${{ inputs.tag }}
          strict: "true"
```

It writes the human-readable report to the log, an annotation for every
`FAIL`, `WARN` and `UNKNOWN`, a summary table, and the JSON report to the
file in the `preflight-report` output. The step's exit code is the
command's. The token (`github-token`) is only read with; checks that need
more than the workflow token can read (secret names, rulesets) are
`UNKNOWN`, which fails with `strict: "true"`. The preflight runs no
repository code, so this step may hold the token.

## Separate build and publish jobs

For anything you publish, run the build in a job that holds no write token
and no tap token, and publish from another job:

1. **Build job** — `permissions: contents: read`, no `environment`, no
   secrets. Run `command: run` with `stages: build,archive,checksum` and
   `github-token: ""`, then upload `dist/` (including
   `dist/hephaestus-manifest.json`) with `actions/upload-artifact`.
2. **Publish job** — `needs: build`, `permissions: contents: write`,
   `environment: release`. Download the artifact into `dist/` and run
   `command: run` with `stages: release,formula,tap` and the tokens.

Jobs run on separate runners, so nothing the build started can observe the
publishing job. A complete file is
[release-split-jobs.yml](../examples/workflows/release-split-jobs.yml).

A single job (the example below) is simpler and still keeps tokens out of
the build step's environment, but it does not isolate them: code from the
build step that keeps running (a daemon started by a build script, for
example) can read a later step's environment. Hephaestus warns when one
process runs build and publishing stages with a token set.

## Untrusted pull requests

The configuration runs commands (`builds[].command`), and a build compiles
code from the checked-out tree. Treat `hephaestus.yaml` like a workflow file:

- **Never run publishing stages (`release`, `tap`, or `run` without
  `stages`) on `pull_request` or `pull_request_target`.** Publish only from
  tag pushes (or a protected `workflow_dispatch`).
- **Never combine `pull_request_target` (or `workflow_run`) with a checkout
  of the pull request's head.** That gives the fork's code your secrets and
  a write token.
- For pull request CI, use `snapshot: "true"`, `stages:
  build,archive,checksum`, `github-token: ""`, no `tap-token`, and
  `permissions: contents: read`.
- Protect `hephaestus.yaml`, build scripts and release notes with
  `CODEOWNERS`. In particular review changes to `builds[].command`,
  `release.github.api_url` / `upload_url`, `notes_file` and template `.Env`
  references.
- Use `concurrency: release-${{ github.ref }}` with `cancel-in-progress:
  false` on the release workflow, so two runs for one tag do not race.

## Example: one job

```yaml
name: Release
on:
  push:
    tags: ["v*"]
permissions:
  contents: read
concurrency:
  group: release-${{ github.ref }}
  cancel-in-progress: false
jobs:
  release:
    runs-on: ubuntu-latest
    environment: release
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - name: Build, archive, checksum
        uses: magicdrive/hephaestus@<full-commit-sha>
        with:
          command: run
          stages: build,archive,checksum
          github-token: ""
      - name: Publish the GitHub Release
        uses: magicdrive/hephaestus@<full-commit-sha>
        with:
          command: release
      - name: Formula and tap pull request
        id: brew
        uses: magicdrive/hephaestus@<full-commit-sha>
        with:
          command: run
          stages: formula,tap
          github-token: ""
          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

Each step continues from the manifest the previous one saved in `dist/`.

## More workflows

Complete files are in [examples/workflows](../examples/workflows/README.md):

| File | Pattern |
| --- | --- |
| `release-single-job.yml` | The example above. |
| `release-split-jobs.yml` | Build in a job with `contents: read`; pass `dist/` (including the manifest) as an artifact to a publishing job with `contents: write` and the `release` environment. Build code never runs where a write token exists. |
| `goreleaser-formula-only.yml` | GoReleaser builds and releases; Hephaestus imports `dist/artifacts.json` and publishes only the Homebrew formula. |
| `matrix-then-publish.yml` | Build natively on several runners (matrix), upload the binaries, then package and publish them in one job with the `existing` provider. |
| `pull-request.yml` | Snapshot build on pull requests without any token. |

Notes for the split pattern: the publishing job trusts the `dist/` artifact
the build job produced (the release stage re-hashes every file against the
manifest, which detects corruption but not a build job that wrote a
consistent but malicious manifest). Keep both jobs in the same workflow run,
triggered only by tags.

## Behavior notes

- Every run of the action downloads and verifies the release binary (about
  5 MB) again; nothing is cached between steps, which other steps could
  modify.
- `GITHUB_SHA` provides `.Commit`; `GITHUB_REPOSITORY` provides the release
  repository unless the configuration names one; `GITHUB_API_URL` provides
  the API URL on GitHub Enterprise Server.
- The exit code is non-zero when any stage fails; outputs and the summary
  are written in every case.
