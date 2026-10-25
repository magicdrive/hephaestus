# Hephaestus v1.1.1

A patch release that fixes problems found while migrating five projects to
v1.1.0. Two of them matter most: the GitHub Action changed the Go toolchain
that your project is built with, and redaction hid paths under the current
directory.

## The Action no longer changes your Go toolchain

The v1.1.0 Action set up the Go that builds Hephaestus with
`actions/setup-go`. That step put Go 1.26.0 first on the job's `PATH` and
exported `GOTOOLCHAIN=local` for the rest of the job. Build commands that
ran after it, including your own `go build` inside the Action, used that Go
instead of the one your workflow set up:

- a module that requires a newer Go failed to build (`go.mod requires
  go >= 1.27.0 (running go 1.26.0; GOTOOLCHAIN=local)`);
- other modules were silently built with Go 1.26.0. This could mean an older
  toolchain than your previous release, or a standard-library vulnerability
  your `govulncheck` reported as fixed.

From v1.1.1, the Go that builds Hephaestus is private to the Action's build
step. It is the version in the `toolchain` line of the Action's `go.mod`
(go1.27.3), and it is obtained in one of two ways:

- **On GitHub-hosted runners** (Linux, macOS and Windows, x64 and ARM64),
  the Action downloads the official archive from `dl.google.com` and checks
  it against a SHA-256 pinned in the Action's source
  (`action/go-toolchain.sha256`). Pinning the Action's commit therefore also
  pins the exact Go that builds Hephaestus. The archive is unpacked under
  `$RUNNER_TEMP` and called by absolute path.
- **Elsewhere**, a `go` (1.21 or later) on `PATH` downloads the same version
  through the module proxy, and the checksum database verifies it.

Nothing is written to `$GITHUB_PATH` or `$GITHUB_ENV`. Before and after the
Action, your steps see the same `PATH`, `go`, `go version`, `GOTOOLCHAIN`
and `GO*` variables. CI checks this on Linux, macOS and Windows, x64 and
ARM64: with Go 1.26 or 1.27 set up by the caller, without `setup-go`, and
for a project that is not written in Go. A control job checks that the same
comparison detects the change v1.1.0 made.

**If your workflow relied on the Action's Go** (a Go build without your own
`actions/setup-go`), add `actions/setup-go` before the Action, as the
[documentation](https://github.com/magicdrive/hephaestus/blob/v1.1.1/docs/github-action.md)
and examples always did. All inputs and outputs are unchanged.

## Redaction no longer hides paths under the current directory

Names containing a `PWD` segment were treated as credentials (as in
`DB_PWD`). This matched the shell's `PWD`, whose value is the current
directory, so every path below it was printed as `***`, for example
`configuration OK: project goreg, root ***`. The same rule also removed
`PWD` from build commands' environment and hid it from `{{ .Env.PWD }}`.

- `PWD` and `OLDPWD` are no longer treated as credentials.
- A variable that looks like a credential only because of its name is no
  longer redacted when its value is the absolute path of an existing file or
  directory (`SSH_AUTH_SOCK`, `GOOGLE_APPLICATION_CREDENTIALS`): it says
  where a credential is, not what it is.
- The tokens Hephaestus knows (`GITHUB_TOKEN`, `HOMEBREW_TAP_GITHUB_TOKEN`,
  `GH_TOKEN`, ...) are redacted whatever their value, as before. Other
  credential-like values are redacted from 8 characters, as before.

## `--fresh` removes the previous run's outputs

`--fresh` (and the Action's `fresh` input) ignored the old manifest but left
its files in `dist/`. A script that packs or uploads `dist/*.tar.gz` could
then pick up an archive of an earlier version.

Before starting, `--fresh` now deletes the archives, checksum files and
formulae that the old manifest records, when this run writes them again. A
file is deleted only if all of these hold:

- it is inside `dist`;
- Hephaestus wrote it;
- its SHA-256 is unchanged since it was recorded.

Imported archives, files outside `dist`, other files in `dist/`, and files
changed since are left alone.

## Compatibility

- **Configuration, CLI and JSON:** no configuration key, command, flag,
  Action input or output, JSON field or check ID changed.
- **The Action's Go:** the Action no longer provides Go to your build
  commands (see above).
- **Template variables:** `{{ .Env.PWD }}` and `{{ .Env.OLDPWD }}` are now
  visible to templates.
- **Build environment:** build commands now receive `PWD` and `OLDPWD`.

## Known limitations

- **Homebrew 4.6 misreads the version.** It parses `64` as the version of
  archive URLs such as `tool_1.2.3_darwin_arm64.tar.gz`. Homebrew 7.0 parses
  `1.2.3`. Generated formulae still omit `version` when Homebrew can read it
  from the URL, because `brew audit --strict` rejects a redundant `version`.
  `brew update`, which delivers tap updates, also updates Homebrew itself.
  An environment that pins an old Homebrew should upgrade it.
- **`hephaestus check` shows the tap pull request branch as its template**
  (`{{ .ProjectName }}-{{ .Version }}`), because `check` has no tag.
  `hephaestus preflight --tag` shows the rendered branch.
- **Testing a formula against local archives** (`file://` URLs) still needs
  a configuration of its own for the test, with
  `HEPHAESTUS_ALLOW_FILE_URLS=1`.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.1.0...v1.1.1
