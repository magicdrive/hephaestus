# Migration playbook: adopting Hephaestus in an existing project

This playbook is a checklist for moving a project's existing release
pipeline (usually GoReleaser) to Hephaestus 1.1. It comes from migrating
five projects in October 2026: git-parallel, kjak, ark, enma and goreg. The
first part applies to every project. The last part records what differed
between them.

[docs/migration/from-goreleaser.md](../migration/from-goreleaser.md)
explains each configuration field. This page covers the work around it:
the audit, compatibility, CI, review and the first release.

## 1. Audit before changing anything

Record these facts before you write any configuration. Read them from the
GitHub API and the repository itself; do not rely on memory.

| What | How |
| --- | --- |
| Latest release, its assets and whether it is immutable | `gh release view --json tagName,isImmutable,assets` |
| Archive names, formats, targets (`386`, `windows/arm64`, ...) | the asset list |
| Archive layout: top-level directory, extra files, modes | `tar -tzvf`, `unzip -l` on the downloaded assets |
| Version string | run the published binary with `--version` |
| Build flags, `CGO_ENABLED`, build tags, the Go toolchain | `.goreleaser.y*ml`, `go version -m <published binary>` |
| The current formula | `gh api repos/<owner>/homebrew-tap/contents/<name>.rb` (read only) |
| Installers that download assets by name | `install.sh`, README, docs |
| Completion scripts and whether the formula installs them | `misc/`, `completions/`, the formula |
| Secrets and environments | `gh secret list`, `gh secret list -e release`, `gh api repos/<r>/environments/release/deployment-branch-policies` |
| Rulesets, default workflow permissions, open pull requests, Renovate | `gh api repos/<r>/rulesets`, `gh api repos/<r>/actions/permissions/workflow`, `gh pr list` |

The environment's deployment policy decides whether a tag can deploy. A
policy of type `branch` named `v*` does not match tags, so a release job
that runs on a tag push and uses the environment will be rejected.
`hephaestus preflight` reports this as `github.environment`.

## 2. Reproduce the last release before releasing anything new

1. Write `hephaestus.yaml` with a `command` build (Step 3 of the GoReleaser
   guide).
2. Check out the commit of the last release and run the local stages
   with that release's tag:
   `hephaestus run --tag vX.Y.Z --stages build,archive,checksum --fresh`
   (this writes nothing to GitHub).
3. Compare the result with the published assets:
   - archive names, formats and targets;
   - members and modes;
   - the checksum file;
   - `--version`;
   - `go version -m` (toolchain, `-trimpath`, ldflags).

   On kjak the binaries were identical byte for byte. The archives still
   differ, as expected: Hephaestus writes uid 0, a fixed mtime and
   directory entries.
4. Keep every difference a user could notice: archive names, Windows
   `tar.gz` versus `zip`, the version string, install paths. If you must
   change one, explain why in the pull request. Do not regenerate past
   releases.
5. Write `scripts/verify-dist.sh`, which checks the expected layout, and run
   it in CI, in the build job and again in the release job before
   publishing. In the release job, set `VERIFY_DIST_NO_EXEC=1` so that no
   built binary runs there.

## 3. The Go toolchain in the GitHub Action

**Up to 1.1.0**, the Action built Hephaestus with `actions/setup-go`, from
its own `go.mod`. setup-go puts that Go version on `PATH` and exports
`GOTOOLCHAIN=local` to the rest of the job. The 1.1.0 Action uses Go 1.26.0.
Your build commands run after this step, so by default they used that Go:

- a module that requires a newer Go fails (`go.mod requires go >= 1.27.0
  (running go 1.26.0; GOTOOLCHAIN=local)`);
- otherwise the release is built, silently, with an older toolchain than
  the one you chose. This can mean a downgrade, or a standard library
  vulnerability that `govulncheck` reported as fixed.

**The fix** ([#22](https://github.com/magicdrive/hephaestus/pull/22),
planned for 1.1.1) builds Hephaestus with a Go private to the Action's build
step: the official archive, checked against a SHA-256 pinned in the
Action's source. Nothing is written to `$GITHUB_PATH` or `$GITHUB_ENV`. With
that version, your build uses the Go that your own `actions/setup-go` step
set up. A Go project must therefore run setup-go before the Action, as the
examples always did.

Whatever the Action version, keep the release toolchain explicit and
checked. The Action is then not the only thing that decides which Go builds
the release. Do both of the following:

```yaml
version: 1
project:
  name: mycli
builds:
  - id: mycli
    matrix:
      os: [darwin, linux]
      arch: [amd64, arm64]
    command: [go, build, -trimpath, -o, "dist/bin/{{ .OS }}_{{ .Arch }}/mycli", .]
    env:
      GOOS: "{{ .OS }}"
      GOARCH: "{{ .Arch }}"
      CGO_ENABLED: "0"
      GOTOOLCHAIN: auto   # the toolchain go.mod selects, not the Action's Go
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli"
```

1. Pin the release toolchain in `go.mod` with the `toolchain` directive, so
   that Renovate can update it. If CI must keep testing with the oldest Go
   that the `go` line allows, do not add `toolchain`: setup-go would prefer
   it. Pin the version in the build instead, with
   `GOTOOLCHAIN: go1.27.3` in `builds[].env`. Track that value with a
   Renovate `customManagers` entry.
2. Assert the toolchain in CI and in the release build job: run
   `go version -m` on a built binary and compare the result with `go.mod`.

## 4. Release workflow

Start from [release-split-jobs.yml](../../examples/workflows/release-split-jobs.yml).
Every migrated project uses three jobs:

| Job | Permissions | Secrets | Purpose |
| --- | --- | --- | --- |
| `ci` | `contents: read` | none | `uses: ./.github/workflows/ci.yml` at the tagged commit. ci.yml has `workflow_call`, and its concurrency group must not cancel itself when called. |
| `build` | `contents: read` | none, no environment | Checks the tag format. Runs the Action with `stages: build,archive,checksum`, `fresh: "true"` and `github-token: ""`, then `verify-dist.sh`, then uploads `dist/`. |
| `release` | `contents: write`, `id-token: write`, `attestations: write` | `environment: release` | Downloads `dist/` and runs `verify-dist.sh` again (no execution). Checks that the tap token is set (as a boolean only). Runs `formula,tap` with `dry-run: "true"`. Then `attest-build-provenance`, `command: release` and `formula,tap`. |

Other rules:

- The workflow sets `permissions: contents: read` at the top level, and
  `concurrency: release-${{ github.ref }}` with `cancel-in-progress: false`.
- Pass tokens only through the Action's `github-token` and `tap-token`
  inputs, never in `env`. The repository's own `GITHUB_TOKEN` publishes the
  release, so the personal access token used with GoReleaser is no longer
  needed.
- Pin every `uses:` to a full commit SHA, followed by a version comment.
- A project without builds (a script shipped as a noarch archive) runs
  `stages: archive,checksum`. Naming `build` fails with "no builds are
  configured".

## 5. CI

Keep every existing job. Add only what is missing from this list:

- **Go checks:** `gofmt -l`, `go vet` (also with `GOOS=windows` when Windows
  is supported), `go mod tidy` diff, staticcheck, `govulncheck`, tests,
  `-race`, a build and a CLI smoke test. Run `-race` on Linux and macOS;
  Windows runners lack cgo by default, so record why it is not run there.
- **Platforms:** a native run for each OS you support. Cross-built targets
  can also be smoke-tested natively: `macos-15-intel`, `ubuntu-24.04-arm`
  and `windows-11-arm` are available. Report cross-built and natively run
  targets separately.
- **Hephaestus:** `command: check`, then a snapshot (`snapshot: "true"`,
  `github-token: ""`), then `verify-dist.sh`.
- **Homebrew E2E** (ubuntu and macOS): generate the formula with a tag,
  point its `url` at the local archives (`file://`) and run `brew style`,
  `brew audit --strict`, install, the smoke test, `brew test` and
  uninstall. When a formula already exists, install the current tap
  formula first, pinned to a tap commit and read only, then
  `brew upgrade`.
- **Workflow and config checks:** actionlint, shellcheck and
  `renovate-config-validator --strict`.
- **Workflow settings:** `permissions: contents: read`, `timeout-minutes`
  on every job, and `cancel-in-progress` for pull requests only.
- **Optional:** one aggregating `ci-gate` job, which is easy to require in
  a ruleset.

Do not delete or weaken a test to get a green run. A flaky test is either
fixed or reported, with its history.

## 6. Renovate

- Keep the existing configuration. Do not turn on automerge.
- The SHA-pinned Action is updated by the `github-actions` manager (keep
  `pinDigests`).
- Add `customManagers` (regex on `# renovate:` comments) for the versions
  in the Makefile and in workflows: `HEPHAESTUS_VERSION`, staticcheck,
  govulncheck, actionlint and Renovate.
- Group the Hephaestus Action and CLI, so that they move together.
- Run the validator with `--strict`, locally and in CI.

## 7. Makefile

- Do not rename or remove existing targets.
- Add `help`, `fmt`, `fmt-check`, `vet`, `lint`, `test`, `test-race`,
  `smoke`, `check`, `snapshot`, `hephaestus-check`, `preflight tag=` and
  `release-check tag=`. Add only what the project needs.
- Run Hephaestus as
  `HEPHAESTUS ?= go run github.com/magicdrive/hephaestus@$(HEPHAESTUS_VERSION)`,
  without other variables in front: scripts that expand
  `$(HEPHAESTUS)` cannot run `VAR=value cmd` prefixes.
- Run `rm -rf dist` before `snapshot`. In 1.1.0, `--fresh` ignores the old
  manifest but does not delete the archives left in `dist/`. In 1.1.1, it
  deletes the archives, checksum files and formulae that the old manifest
  records. `rm -rf dist` still clears everything else.
- `make publish` (pushing a tag) runs `preflight` first. With `--strict`
  it stops on any WARN; some projects run the non-strict version until
  their ruleset WARNs are resolved.

## 8. Pull request and review

### Safe pushes

During this migration, one push reached `main` instead of the pull request
branch, and a revert pull request undid it. The cause: the clone has
`push.default=tracking`, and the worktree was created from `origin/main`
without `--no-track`. The branch therefore tracked `main`, and
`git push -u origin <branch>` sent it there. A ruleset that blocks only
deletion and force pushes did not stop it. Do not rely on the ruleset.

1. Create worktrees with `--no-track`:
   `git worktree add --no-track -b <branch> <dir> origin/main`.
2. Never run a bare `git push`. Always name the destination:
   `git push origin HEAD:refs/heads/<branch>`.
3. Before pushing, print and check:
   - the local branch and `HEAD`, the remote URL, `push.default` and the
     upstream;
   - the pull request's number, state, head ref and base ref.

   The local branch must equal the pull request's head ref, and must not be
   `main`. The remote head must be an ancestor of `HEAD` (fast-forward
   only; never force).
4. After pushing, check that `git ls-remote origin refs/heads/main` is
   unchanged and that the pull request's head is the new `HEAD`.

Put these rules in every brief you give to another agent or contributor.

### Pull request contents

- The pull request lists:
  - a before/after summary;
  - a compatibility table;
  - the Homebrew checks that ran;
  - the conditions for the first release.

## 9. First release and recovery

1. Merge the pull request. Close the Renovate pull requests that update
   GoReleaser.
2. Update the notes (`notes_file` or `generate_notes`). Run
   `make preflight tag=vX.Y.Z`.
3. Push the tag. The workflow publishes the release, then opens a pull
   request on the tap.
4. Review and merge the tap pull request: `brew style`,
   `brew audit --strict`, the URLs and the sha256 values.
5. If a step fails, run `hephaestus status --tag vX.Y.Z`
   ([recovery](../recovery.md)) and re-run the workflow. Never delete or
   re-create a published release or tag. To roll back the migration
   itself, revert its pull request.

### Updating the Action pin

Pin the Action only to a commit that a release tag points to, never to an
unreleased commit. Use the commit of the tag (`git rev-parse vX.Y.Z^{commit}`),
with a `# vX.Y.Z` comment. Set the same version in the Makefile's
`HEPHAESTUS_VERSION`.

To verify an unreleased fix in a project's CI, push a commit that pins the
fix's commit, with its Go pseudo-version as the comment and as
`HEPHAESTUS_VERSION`. Once CI is green, revert that commit, so that the pull
request never asks to merge an unreleased pin. The five migration pull
requests verified #22 this way.

## 10. Per-project differences

| Project | Shape | Notable choices |
| --- | --- | --- |
| git-parallel | Bash script, no build | A noarch archive, `stages: archive,checksum`, and a new formula (none existed). The version is embedded in the script and checked against the tag. The `release` environment allows branch `v*` only, so the owner must add tag `v*`. |
| kjak | Go, 5 targets, windows zip | Binaries byte-identical to v0.0.3. `GOTOOLCHAIN: auto` with `toolchain` in go.mod. Main was pushed by mistake and reverted (#12, then #13 stacked). |
| ark | Go, 8 targets incl. 386, windows tar.gz | All existing CI jobs are kept. Builds go through `.github/release/build.sh`, which exports `GOTOOLCHAIN` from go.mod, asserts it, and sets `cache.release` only for a clean tree at the tag. `release_workflow_test.go` checks the new workflow. The formula's `desc` is over 80 characters, kept for compatibility. The full test suite does not pass on Windows yet (pre-existing). |
| enma | Go, 8 targets incl. 386, windows zip, `install.sh` | Installer E2E against snapshot archives. Completions are shipped as files. `GOTOOLCHAIN: go1.27.3` is pinned in `builds[].env`, not in go.mod, so tests still run with the minimum Go. Without this pin, the release would have been built with an older Go than v2.1.0. |
| goreg | Go, 8 targets incl. 386, windows tar.gz | Shipped completions (bash, zsh) installed with `completions.files`. The toolchain is pinned to fix a standard-library vulnerability. |

## 11. Gaps found in Hephaestus 1.1.0

The projects work around each of these. Where one is fixed, the fix is in
[#22](https://github.com/magicdrive/hephaestus/pull/22), planned for 1.1.1.

- **The Action leaks its Go into the caller's job** (§3). It puts its own Go
  on `PATH` and exports `GOTOOLCHAIN=local`, so build commands run with the
  Action's Go. *Fixed in #22.*
- **`--fresh` leaves old archives in `dist/`.** *Fixed in #22*: the outputs
  that the old manifest records are deleted.
- **Testing a generated formula against local archives takes a separate
  configuration.** `run --snapshot` skips the formula stage, and
  `url_template` cannot read an optional variable, so the project's own
  configuration cannot switch to `file://` URLs. The projects either rewrite
  the formula's `url` with sed, or (ark) generate a test configuration: the
  snapshot archives are imported with `existing`, `url_template` is set to
  `file://` URLs, `HEPHAESTUS_ALLOW_FILE_URLS=1` is set, and the build and
  formula stages run with a tag.
- **Old Homebrew versions misread the version.** The generated formula
  omits `version` when Homebrew can infer it from the URL. Homebrew 4.6.20
  infers `64` from platform-specific archive names such as
  `tool_1.2.3_darwin_arm64.tar.gz`; Homebrew 7.0.10 infers `1.2.3`. Both
  results were checked. *Not changed*: `brew audit --strict` rejects a
  redundant `version`, and `brew update`, which delivers tap updates, also
  updates Homebrew itself.
- **The shell's `PWD` is redacted as a credential.** `PWD` matches the
  credential-name heuristic: a `PWD` segment, meant for "password". Its
  value is the current directory, so every path under the current
  directory is printed as `***`. For example, `hephaestus check` prints
  `root ***`. `env -u PWD` works around it. *Fixed in #22*: `PWD` and
  `OLDPWD` are exempt. A value matched only by its variable's name is not
  redacted when it is an existing path. Known tokens are still always
  redacted.
- **`hephaestus check` shows the tap pull request branch unexpanded**
  (`{{ .ProjectName }}-{{ .Version }}`). *By design*: `check` has no tag.
  `preflight --tag` shows the branch rendered.
- **The noarch docs have no example of the stages for a project without
  builds.**
