# GoReleaser compatibility fixtures

These files record what a **real, pinned GoReleaser** produced for a small
project, so Hephaestus' GoReleaser-compatible `brews` handling can be compared
against actual GoReleaser output instead of against our reading of its docs.

Nothing in here was written by hand except `project/` and this README. The
`v2.19.0/` directory is generated output and must only be changed by
regenerating it.

## Layout

```
project/                 input: minimal Go CLI + .goreleaser.yaml
v2.19.0/
  homebrew/*.rb          every formula GoReleaser wrote to dist/homebrew/
  artifacts.json         dist/artifacts.json, verbatim
  metadata.json          dist/metadata.json, verbatim
  checksums.txt          dist/checksums.txt, verbatim
  archives/<name>.list   one per archive: "<mode> <path>" for each entry, sorted by path
```

Archives and binaries are not kept (size); only their listings are.

## Regenerating

```
scripts/goreleaser-compat-fixture.sh
```

The script:

1. downloads the GoReleaser `v2.19.0` release asset for the host platform and
   `checksums.txt` with `gh release download` into an empty temporary
   directory, and refuses to continue unless the asset's sha256 matches;
2. extracts the `goreleaser` binary into a separate directory and checks that
   `goreleaser --version` reports `2.19.0`;
3. copies `project/` into a throw-away git repository (fixed author, committer
   and dates, global/system git config ignored), commits it, creates the
   annotated tag `v1.2.3`, and adds a placeholder `origin` remote
   (`https://github.com/acme/compatcli.git`) because GoReleaser refuses to run
   without one; the remote is never contacted;
4. runs, with `GITHUB_TOKEN=dummy-not-used`:

   ```
   goreleaser release --clean --parallelism=1 --skip=publish,announce,validate
   ```

   so the release has the real version `1.2.3` (not a snapshot version), and
   nothing is uploaded, pushed or announced;
5. replaces `v2.19.0/` with the files listed above.

Requirements: `gh` (authenticated, only for the download), `go`, `git`, `tar`,
`shasum` or `sha256sum`. Runs on darwin/linux, amd64/arm64.

## Recorded environment

| Item | Value |
| --- | --- |
| GoReleaser | v2.19.0 (`GitCommit 00139d15712716a3a2f21eb4d533f231ff19b9c5`, `goreleaser_Darwin_x86_64.tar.gz`) |
| Go toolchain used for the builds | go1.27.2 darwin/amd64 |
| Host | macOS (Darwin 24.6.0), x86_64 |
| Recorded | 2026-10-24 |

## Cases

`project/.goreleaser.yaml` is built to exercise the following:

- **Default `install`** (`brews[0]`, formula `compatcli`). No `install` is
  set, so GoReleaser guesses it from the archive's binaries. The `default`
  archive contains both builds; `helper` is built for darwin only, so darwin
  archives contain `compatcli` and `helper` and linux archives contain only
  `compatcli`. Result: the `on_macos` blocks install both binaries and the
  `on_linux` blocks install only `compatcli` - the guessed install is
  per-platform, not global. Each block uses `define_method(:install)`.
  (`allow_different_binary_count: true` is required for this archive layout;
  GoReleaser rejects it otherwise.)
- **Release repository differs from the source repository.** `release.github`
  is `acme-dist/compatcli-releases` while the git remote is `acme/compatcli`;
  every formula `url` points at
  `https://github.com/acme-dist/compatcli-releases/releases/download/v1.2.3/...`.
- **Tap repository with no `directory` and no `pull_request`.** Recorded in the
  `BrewConfig` echo in `artifacts.json`; publishing is skipped, so nothing about
  the tap commit itself is observable here.
- **`caveats` containing a Ruby interpolation** (`#{HOMEBREW_PREFIX}`). It is
  emitted verbatim inside a `<<~EOS` heredoc in `def caveats` (so Ruby
  interpolates it at install time); it is not escaped.
- **`version` line.** Both formulae contain `version "1.2.3"`.
- **`license`, `homepage`, `description` (`desc`), `test`.**
- **darwin with amd64 only** (`brews[1]`, formula `compatcli-intel`).
  `brews.ids` selects archives, not architectures, so the amd64-only case uses
  its own build (`compatcli-intel`, darwin+linux amd64) and archive (`intel`).
  Result: `on_macos` has a single unconditional `url`/`sha256`/install (no
  `Hardware::CPU.intel?` guard), followed by an `if Hardware::CPU.arm?` block
  defining a Rosetta-style `def caveats` ("The darwin_arm64 architecture is not
  supported for the CompatcliIntel formula at this time..."). The user caveats
  are then defined again by the top-level `def caveats`, which, being later in
  the class body, is the definition Ruby keeps.

The builds use `-trimpath`, `mod_timestamp: "{{ .CommitTimestamp }}"` and
archive `builds_info.mtime: "{{ .CommitDate }}"`, and the run uses
`--parallelism=1` (archive member order follows build completion order
otherwise). These only make archives byte-reproducible for a given toolchain;
they do not change the formulae beyond their `sha256` values.

## Values that are environment-dependent or not comparable

These were left exactly as GoReleaser wrote them. Comparisons must ignore or
normalize them:

- **`sha256` values** (formulae, `checksums.txt`, `Checksum` in
  `artifacts.json`). Stable across runs with the same Go toolchain (verified by
  three consecutive runs), but the binaries - and therefore the archives -
  change with the Go version and possibly the host. Compare structure, not
  digests, unless Hephaestus is fed the same archives.
- **`metadata.json` `date`**: wall-clock time of the run, in the host's local
  time zone.
- **`metadata.json` `runtime`**: GOOS/GOARCH of the machine that ran GoReleaser.
- **`metadata.json` `commit`**: hash of the throw-away commit. Deterministic for
  unchanged `project/` contents (fixed identities and dates), changes whenever
  any project file changes.
- **`artifacts.json` `path`**: relative to the throw-away repository
  (`dist/...`); there are no absolute paths. The `dist/<build>_<target>/`
  directory names are GoReleaser internals.
- **`artifacts.json` `internal_type`**: GoReleaser's internal enum, meaningless
  outside GoReleaser.
- **`artifacts.json` ordering** and the order of `extra.Binaries`: follow
  GoReleaser's pipeline order (stable here only because of
  `--parallelism=1`).
- **`artifacts.json` `BrewConfig`**: GoReleaser's in-memory config after
  defaults (e.g. `commit_author: goreleaserbot`, `goarm: "6"`,
  `goamd64: "v1"`); useful for reading GoReleaser's defaults, not a format
  Hephaestus produces.
- **Archive entry owners and timestamps**: deliberately dropped from the
  `.list` files; only mode and path are kept.
- **Tap side effects** (commit message, author, branch, pull request): not
  recorded, because publishing is skipped.

GoReleaser v2.19.0 prints `DEPRECATED: brews should not be used anymore` for
this configuration (it prefers `homebrew_casks`); the formulae are still
generated.
