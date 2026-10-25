# Hephaestus v1.2.0

The GitHub Action no longer compiles
Hephaestus: it installs the release binary that belongs to the commit you
pinned, verified by its checksum and its provenance attestation, and leaves
your toolchain alone. This release also brings `hephaestus publish`, and the
fixes prepared for v1.1.1 (never released).

## The Action runs the verified release binary

```yaml
- uses: magicdrive/hephaestus@<full commit SHA of v1.2.0> # v1.2.0
```

Before your build runs, the Action:

1. reads the release named in `action/VERSION` of the pinned commit; the
   release workflow refuses a tag that this file does not name;
2. downloads `hephaestus_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows)
   and `hephaestus_checksums.txt`, from that release only;
3. checks the archive's SHA-256;
4. verifies the archive's provenance attestation (`gh attestation verify`).
   It must have been signed by `magicdrive/hephaestus`'s `release.yml` on a
   GitHub-hosted runner, for the release tag and, when pinned by SHA, for
   exactly the pinned commit;
5. extracts only the regular file `hephaestus[.exe]` at the archive root,
   and checks that it reports the expected version.

Any failure stops the Action. It never falls back to building from source.

As a result:

- **No toolchain is involved.** Nothing is compiled, no Go is downloaded or
  set up, and nothing is written to `$GITHUB_PATH` or `$GITHUB_ENV`. Your
  build commands use the toolchain your workflow set up. A runner without Go
  can run the Action for a project in any language.
- **The binary is bound to your pin.** The attestation proves that the
  binary was built by Hephaestus's release workflow from the commit you
  pinned.
- **Supported platforms are unchanged:** Linux, macOS and Windows, on x64
  and ARM64. A runner needs `bash`, `curl`, `tar` and `gh`, which
  GitHub-hosted runners have.

**Reference the Action by a release.** Use the full SHA of a release tag's
commit (recommended) or the tag itself. A branch such as `@main` now fails,
because no release binary belongs to it.

## `hephaestus publish`

The tool that releases Hephaestus is now a command for any project:
`make publish vX.Y.Z` that runs `hephaestus publish`. In order, it:

1. checks that the release can be made: branch, clean worktree, in sync
   with origin, the tag unused, CI green on the commit, rulesets, the
   release environment, the tap, and the generic preflight;
2. asks for confirmation;
3. pushes an annotated tag;
4. follows the release workflow;
5. verifies the release: assets, checksums, attestations and the tap pull
   request.

See [docs/publish.md](https://github.com/magicdrive/hephaestus/blob/v1.2.0/docs/publish.md).

The Action gains `command: plan`, with the outputs `prerelease` and `tap`.
The steps that use the tap token can then run only when the tap is
published. A prerelease never even receives the token.

## Fixes (prepared for v1.1.1)

- **The Action no longer changes your Go toolchain.** v1.1.0 put its own Go
  first on `PATH` and exported `GOTOOLCHAIN=local`, so your Go modules were
  built with Go 1.26.0, or failed to build.
- **`PWD` is no longer redacted as a credential.** Paths under the current
  directory are no longer printed as `***`. A value matched only by its
  variable's name is not redacted when it is the path of an existing file or
  directory. Known tokens are always redacted.
- **`--fresh` removes the previous run's outputs** (archives, checksum files
  and formulae) from `dist`, if they are unchanged and Hephaestus wrote them.

## Release candidate

`v1.2.0-rc.1` (commit `bae0e2b`) was published as a prerelease. Before this
release, it was verified on GitHub in two ways:

- **The Action's release path, pinned to that commit.** The Action
  downloaded the release binary, checked its checksum, verified its
  attestation, extracted it and checked its version. This ran on Linux,
  macOS and Windows, x64 and ARM64. The job's toolchain was unchanged
  afterwards.
- **The five projects that adopted Hephaestus.** git-parallel, kjak, ark,
  enma and goreg ran CI, snapshots and Homebrew end-to-end tests.

v1.2.0 differs from the release candidate only in `action/VERSION`, these
notes, the changelog and CI.

## Compatibility

- **Unchanged:** configuration, CLI flags, Action inputs and outputs, JSON
  fields and check IDs. New: the `publish` command, and the Action's `plan` command with the outputs `prerelease` and `tap`.
- **The Action:** it must be referenced by a release tag or that tag's
  commit, and it no longer provides Go. Set up your own Go, as the examples
  always did.
- **Template variables:** `{{ .Env.PWD }}` and `{{ .Env.OLDPWD }}` are now
  visible to templates.

## Known limitations

- Homebrew 4.6 parses the version of archive URLs such as
  `tool_1.2.3_darwin_arm64.tar.gz` as `64`; Homebrew 7.0 parses it
  correctly. `brew update` updates Homebrew itself.
- `hephaestus check` shows the tap pull request branch as a template;
  `hephaestus preflight --tag` renders it.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.1.0...v1.2.0
