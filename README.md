# Hephaestus

**Forge release artifacts into reliable distributions.**

[日本語](README_ja.md) · [Configuration](docs/configuration.md) ·
[Archives](docs/archives.md) · [GitHub Action](docs/github-action.md) ·
[GoReleaser compatibility](docs/compatibility/goreleaser.md) ·
| [Compatibility policy](docs/compatibility/policy.md) | SemVer, the stable surface, deprecation |
[Architecture](ARCHITECTURE.md)

Hephaestus is release automation for command-line tools written in any
language. It takes the executables your build produces — or builds them by
running your own command per target — packages them into reproducible
archives, writes a checksum file, publishes a GitHub Release and generates a
Homebrew formula that it delivers to your tap by pull request.

It is a single Go binary and a GitHub Action, configured by one
`hephaestus.yaml`.

## Why Hephaestus

Shipping a CLI is mostly the same work in every language: per-platform
binaries, archives, checksums, a GitHub Release, a Homebrew formula. Doing it
by hand or with ad-hoc scripts goes wrong in familiar ways:

- **Archives contain what happened to be there.** A glob picks up a stray
  `.env`, a file moves when another file is added, two entries overwrite
  each other on macOS.
- **The same commit produces different bytes.** Timestamps, file order and
  permissions vary by machine, so checksums cannot be reproduced.
- **Releases are half published.** A failed upload leaves a public release
  with missing assets; a retry creates a duplicate release or asset.
- **The formula points at something else.** A URL or checksum is copied from
  a naming convention instead of from the file that was uploaded, or
  `bin.install` names a path that is not in the archive.
- **Tokens are everywhere.** The build, which runs third-party code, holds
  the token that can rewrite the release and the tap.

Hephaestus's answers:

| Problem | What Hephaestus does |
|---|---|
| Unclear archive contents | `files` is the complete, declared list with exact path semantics; collisions, case-only collisions, unsafe modes and credential-looking files are errors |
| Irreproducible bytes | Sorted entries, fixed timestamps (`SOURCE_DATE_EPOCH`), normalized modes and ownership, fixed compression headers |
| Half-published releases | Draft first, upload and verify every asset, publish last; a re-run resumes and identical assets are no-ops |
| Wrong formula data | URLs and checksums come from the Artifact Manifest; `install` paths are verified against each archive's recorded contents |
| Over-shared tokens | Release and tap tokens are separate; they are removed from build commands' environment, and the action builds in a step without tokens |

## Philosophy

- **Plan, then write.** Every input of every selected stage is checked before
  anything runs; nothing is written when any target fails to plan.
- **Never invent data.** The manifest records only what was observed: digests
  are computed from the bytes on disk, URLs only after a verified upload.
- **Declare, do not guess.** Archives contain exactly what you list. There
  are no implicit README/LICENSE files and no heuristics.
- **Language-agnostic.** Hephaestus does not compile anything itself. It runs
  your command, imports files you built, or imports GoReleaser's output.
- **Safe to re-run.** Every publishing step reads before it writes.

## How it works

```text
hephaestus.yaml
      │
      ▼
config + preflight      strict YAML, every template checked; every stage's
      │                 inputs verified before anything runs
      ▼
build       command per target │ existing files │ GoReleaser artifacts.json
      ▼
archive     declared files → byte-reproducible tar.gz / zip
      ▼
checksum    <project>_<version>_checksums.txt (sha256sum format)
      ▼
release     GitHub Release: draft → upload + verify → publish
      ▼
formula     Homebrew formula from manifest checksums and contents
      ▼
tap         pull request (default) or direct commit to your tap

            every stage reads and writes dist/hephaestus-manifest.json
```

Stages always run in this order. Each can also run alone, or in a later job:
it continues from the manifest. Details:
[docs/architecture/overview.md](docs/architecture/overview.md).

## Quick start

**1. Install** — see [Installation](#installation).

**2. Configure** — `hephaestus.yaml` at the repository root. Start from a
template for your kind of project (`go`, `rust`, `script` for a noarch
Bash/Python/Node CLI, `prebuilt` for executables built elsewhere); it is
never written over an existing file:

```sh
hephaestus init --template go --repo acme/mycli
```

or write it by hand:

```yaml
version: 1

project:
  name: mycli
  description: Friendly tool for managing widgets
  homepage: https://github.com/acme/mycli
  license: MIT

builds:
  - id: mycli
    matrix:
      os: [darwin, linux, windows]
      arch: [amd64, arm64]
    command: [go, build, -trimpath, -o, "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}", .]
    env:
      GOOS: "{{ .OS }}"
      GOARCH: "{{ .Arch }}"
      CGO_ENABLED: "0"
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"

archives:
  - id: default
    format_overrides:
      - os: windows
        format: zip
    files:
      - src: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"
        dst: "mycli{{ .Ext }}"
        mode: "0755"
      - LICENSE
      - README.md

release:
  github:
    owner: acme
    name: mycli

brews:
  - name: mycli
    directory: Formula
    repository:
      owner: acme
      name: homebrew-tap
```

**3. Check and try it locally**:

```sh
hephaestus check
hephaestus run --snapshot --stages build,archive,checksum
ls dist/
```

**4. Release** — push a tag and run the [GitHub Action](#github-action), or:

```sh
GITHUB_TOKEN=... HOMEBREW_TAP_GITHUB_TOKEN=... hephaestus run --tag v1.2.3
```

More configurations: [examples/](examples/) (Go, Rust, Zig, C++, GoReleaser,
prebuilt artifacts, workflows).

## Installation

With Homebrew (the latest stable release whose formula pull request has
been merged into [magicdrive/homebrew-tap](https://github.com/magicdrive/homebrew-tap);
prereleases are not in the tap):

```sh
brew install magicdrive/tap/hephaestus
brew upgrade hephaestus                        # later, to update it
```

or download an archive from the GitHub Releases page; release archives and
the checksum file carry build provenance attestations
(`gh attestation verify <file> --repo magicdrive/hephaestus`).

In GitHub Actions, use the action. It runs the verified release binary that
belongs to the commit you pin, without installing a toolchain:

```yaml
- uses: magicdrive/hephaestus@<full commit SHA of a release tag> # vX.Y.Z
```

From source (Go 1.26 or later):
`git clone https://github.com/magicdrive/hephaestus.git && cd hephaestus && make build`
(the binary is `build/hephaestus`).

### Install script (macOS and Linux, amd64 and arm64)

Download the script, read it if you like, then run it with `sh`:

```sh
curl -fsSLO https://raw.githubusercontent.com/magicdrive/hephaestus/main/install.sh
sh install.sh                                  # the latest release, into ~/.local/bin
"$HOME/.local/bin/hephaestus" --version        # prints: hephaestus vX.Y.Z
```

```sh
sh install.sh                                  # install the latest release
sh install.sh --version v1.3.0                 # install a given release (older ones too)
sh install.sh --upgrade                        # switch to the latest stable release
sh install.sh --upgrade --version v1.3.0       # switch to a given release (also a downgrade)
sh install.sh --dir "$HOME/.local/bin"         # choose the directory (this is the default)
sh install.sh --help
```

- **Where**: `$HOME/.local/bin/hephaestus` by default; `--dir DIR` installs `DIR/hephaestus` and creates
  `DIR` if needed (spaces in `DIR` are fine). It never uses `sudo` and never changes `PATH` or
  shell start-up files: if the script says the directory is not in `PATH`, add it, e.g.
  `export PATH="$HOME/.local/bin:$PATH"` in `~/.profile`. For a system directory such as
  `/usr/local/bin`, run it as a user who can write there (it does not ask for `sudo` itself).
- **Versions**: without `--version` it installs the latest stable release (pre-releases are not
  picked). `--version` takes a release tag, `v1.3.0` or `1.3.0`, newer or older than the
  installed one; a version that does not exist is an error, never another version. `--upgrade`
  is the same run, named for what you mean. Running it again with the installed version changes
  nothing. Every hephaestus release can be installed (v0.1.0 and later, pre-releases with `--version`).
- **Updating**: run `sh install.sh --upgrade` again (with the same `--dir` if you used one).
- **Checks**: the archive must match the SHA-256 listed in the release's checksum file
  (`hephaestus_checksums.txt`); a missing or malformed checksum file stops the install. Only the
  `hephaestus` binary is taken out of the archive, after its entries have been checked (no absolute
  paths, `..`, links or devices). The new binary must run and report the version asked for
  before it replaces the old one in a single rename: a failed or interrupted run leaves the
  installed `hephaestus` as it was.
- **Security**: every request starts at github.com, over https, and redirects (GitHub sends the
  downloads to its asset host) are followed only to https. The checksum file comes from the same GitHub release as the archive, so it catches a
  broken or altered download, but not a release that was itself replaced on GitHub. For a
  stronger check, verify the archive's build provenance with the
  [GitHub CLI](https://cli.github.com/) before you trust it:
  `gh attestation verify hephaestus_<version>_<os>_<arch>.tar.gz -R magicdrive/hephaestus`
  (releases from v0.1.0 on have these attestations).
  The command above runs the script from the `main` branch; to run a fixed script, download it
  from a release tag instead (`https://raw.githubusercontent.com/magicdrive/hephaestus/<tag>/install.sh`;
  the tags from v1.5.0 on have it). Piping it straight into `sh` (`curl ... | sh`) also works, but then you
  run what you have not seen.
- **Homebrew**: the script refuses Homebrew's directories (its prefix, e.g. `/opt/homebrew`, or
  `/usr/local` on an Intel Mac) and does not replace a symbolic link such as one Homebrew made.
  Keep one way of installing: with Homebrew, upgrade with `brew upgrade hephaestus`; the script warns
  if another `hephaestus` comes first in `PATH`.
- **Uninstall**: delete the file: `rm "$HOME/.local/bin/hephaestus"` (or `DIR/hephaestus`).
- **Needs**: a POSIX `sh`, `curl` (or GNU Wget 1.17+; BusyBox wget is refused because it does
  not check certificates, and wget2 is not supported), `tar`, `gzip`, and `sha256sum`, `shasum`
  or `openssl`. A proxy set in `https_proxy` is used. `INSTALLER_TIMEOUT` (seconds, default 60)
  is how long it waits for a connection or more data.
- **Windows**: the script does not run on Windows (Git Bash, MSYS2 and Cygwin are refused). Use
  the `hephaestus_<version>_windows_<arch>.zip` of the GitHub Releases page (it holds `hephaestus.exe`), or the action in GitHub Actions. Inside WSL, the Linux instructions apply.
- **Troubleshooting**: *not writable* — choose a directory of your own with `--dir`;
  *not in PATH* — see above; *does not run in DIR (noexec)* — choose a directory on another file
  system; *SHA-256 mismatch* or *rejected* — nothing was installed; try again later and report it
  if it persists; *release not found* — check the version on the
  [Releases page](https://github.com/magicdrive/hephaestus/releases); *inside the Homebrew prefix* —
  use `brew upgrade hephaestus`, or another `--dir`.

## Usage

```text
hephaestus init --template go  write a starting hephaestus.yaml (go, rust, script, prebuilt)
hephaestus check              validate the configuration and print the plan
hephaestus run                run every configured stage (or --stages a,b)
hephaestus build|archive|checksum|release|formula|tap   run one stage
hephaestus preflight --tag vX.Y.Z   read-only release checks before pushing a tag
hephaestus status --tag vX.Y.Z      read-only state of a release and the next safe action
hephaestus publish vX.Y.Z           check, tag, push, then follow and verify the release workflow
hephaestus version
```

| Flag | Meaning |
|---|---|
| `--config PATH` | Configuration file (default: `hephaestus.yaml` in the current directory). |
| `--tag TAG` | Release tag (default: `$GITHUB_REF_NAME` on tag pushes, except with `--snapshot`). Required unless `--snapshot`. |
| `--snapshot` | Version `0.0.0-SNAPSHOT`, no tag. Configured release, formula and tap stages (they need a tag) are skipped; naming them in `--stages` is an error. |
| `--stages LIST` | `run` only: comma-separated stages or `all`. |
| `--phase local\|publish` | `run` only: run just the selected stages of one phase, `local` (build, archive, checksum) or `publish` (release, formula, tap), so building and publishing can run in separate processes (the action does this). |
| `--dry-run` | Plan release and tap writes without performing them. |
| `--fresh` | Start a new manifest instead of continuing the existing one. |
| `--commit SHA` | Commit (default: `$GITHUB_SHA`). |
| `--repo OWNER/NAME` | Release repository (default: configuration, then `$GITHUB_REPOSITORY`). |
| `--token-env NAME`, `--tap-token-env NAME` | Variables holding the tokens (defaults `GITHUB_TOKEN`, `HOMEBREW_TAP_GITHUB_TOKEN`). |

Each run prints the SHA-256 of every archive, checksum file and formula and
ends with `==> published`, `no-op`, `dry-run` or `failed`.

## Shell completion

`hephaestus completion bash|zsh|fish` prints a completion script for
commands, flags and fixed flag values (`--format`, `--template`,
`--stages`, `--phase`, `--skip`); details in
[docs/cli.md](docs/cli.md#completion).

**With Homebrew** (from v1.1.0, including `brew upgrade` from 1.0.0), the
formula installs all three scripts into Homebrew's completion directories:

| Shell | File installed | To enable it, once |
| --- | --- | --- |
| bash | `$(brew --prefix)/etc/bash_completion.d/hephaestus` | Install `bash-completion@2` and load it from `~/.bash_profile`: `[[ -r "$(brew --prefix)/etc/profile.d/bash_completion.sh" ]] && . "$(brew --prefix)/etc/profile.d/bash_completion.sh"` |
| zsh | `$(brew --prefix)/share/zsh/site-functions/_hephaestus` | In `~/.zshrc`, before `compinit`: `FPATH="$(brew --prefix)/share/zsh/site-functions:${FPATH}"`, then `autoload -Uz compinit && compinit` |
| fish | `$(brew --prefix)/share/fish/vendor_completions.d/hephaestus.fish` | Nothing for a fish installed by Homebrew; another fish needs that directory in `$fish_complete_path` |

Installing the file and enabling completion are separate: Homebrew only
installs the files, and the shell setup above is yours (many setups already
have it, as Homebrew's own documentation recommends it). After an upgrade,
start a new shell; zsh may keep a cached function list until
`rm -f ~/.zcompdump*; compinit`.

**Without Homebrew**, load the script for the session or install it:

```sh
eval "$(hephaestus completion bash)"                                    # bash
hephaestus completion zsh > ~/.zfunc/_hephaestus                          # zsh, with fpath=(~/.zfunc $fpath) before compinit
hephaestus completion fish > ~/.config/fish/completions/hephaestus.fish   # fish
```

## Configuration

One YAML file; unknown keys are errors with a line number and a suggestion,
and every template is test-rendered before any work starts. The full
reference, including defaults, constraints and template variables, is
[docs/configuration.md](docs/configuration.md).

## Custom builds

The `command` provider runs your command once per target, with the target in
template variables (`{{ .OS }}`, `{{ .Arch }}`, `{{ .Ext }}`) and environment
variables (`HEPHAESTUS_OS`, `HEPHAESTUS_ARCH`, `HEPHAESTUS_ARM`,
`HEPHAESTUS_OUTPUT`). A list is executed without a shell; a string runs with
`sh -c` (or `cmd /C` on Windows). The command must create `output`.

Hephaestus does not cross-compile: declaring a target only tells your command
what to build. Examples for [Go](examples/go/README.md),
[Rust](examples/rust/README.md), [Zig](examples/zig/README.md) and
[C++](examples/cpp/README.md).

## GoReleaser integration

- **Import GoReleaser's output**: a build with `goreleaser:` reads
  `dist/artifacts.json` and records its binaries (and, with `archives: true`,
  its archives). Every file is re-hashed and GoReleaser's checksum verified.
- **Copy `brews`**: Hephaestus reads GoReleaser's `brews` field names. Each
  field's status — supported, different, rejected — is listed in
  [docs/compatibility/goreleaser.md](docs/compatibility/goreleaser.md).
- **Migrate step by step**, from "GoReleaser releases, Hephaestus publishes
  the formula" to a full migration:
  [docs/migration/from-goreleaser.md](docs/migration/from-goreleaser.md),
  [examples/goreleaser-formula-only](examples/goreleaser-formula-only/README.md).

## Existing artifact integration

The `existing` provider takes files built elsewhere — another job, a native
runner per platform, another build system — from a path template per target.
With `kind: archive` it imports release-ready `.tar.gz`/`.zip` archives,
inspected (never extracted) and rejected if they contain traversal, links,
special files or unsafe modes. See
[examples/existing-artifacts](examples/existing-artifacts/README.md).

## Archives

`files` is the complete content list of each archive, with exact rules for
literal paths, globs, renames, `strip_parent`, exclusions, per-target
conditions and modes. Output is byte-for-byte reproducible. Reference:
[docs/archives.md](docs/archives.md).

## GitHub Releases

- Every asset is re-hashed against the manifest before anything is sent.
- A new release is created **as a draft**, assets are uploaded and verified
  against the size and digest GitHub reports, and only then is the release
  published — compatible with immutable releases.
- Re-running is safe: an identical asset is left alone, an incomplete one is
  uploaded again, a different one is an error unless
  `replace_existing_assets: true` on a draft. A lost response is followed by
  a lookup, never by a duplicate create. A published release's assets are
  never replaced or deleted, and no release is ever deleted.
- A published release is never edited: its title, notes and prerelease flag
  are set while it is a draft and published with it; a re-run that would
  change them only reports the difference.
- `prerelease: auto` marks semantic-version prereleases; `make_latest`,
  `name_template`, `notes`, `notes_file` and `generate_notes` (GitHub-generated
  notes for a new release) are supported; GitHub Enterprise Server through
  `api_url`/`upload_url`.

## Homebrew formula

Formulae are generated from the manifest: URLs from the published release
(or `url_template`), SHA-256 from the archives. The output follows current
Homebrew style (`on_macos`/`on_linux` with `on_arm`/`on_intel`, one
`def install`, SPDX `license` expressions) and is checked in CI with
`brew audit --strict`, `brew install` and `brew test`. Every value is an
escaped Ruby literal; `install` paths are verified against each archive's
contents; a default `test` block checks that installed executables exist.
`brews[].completions` installs bash, zsh and fish completions, generated by
the installed executable or shipped in the archive, for a CLI in any
language ([configuration](docs/configuration.md#formula-completions)).

## Tap publishing

- **Pull request mode (default)**: the formula is committed to a branch
  (`<project>-<version>`) and a pull request is opened, or an open one
  reused; a pull request against another repository (for example the
  upstream of a fork) is supported.
- **Direct mode** (`repository.pull_request.enabled: false`): one commit to
  the configured or default branch.
- Identical content creates no commit and no pull request. References are
  only created, never force-updated. Concurrent updates are detected with the
  file's blob SHA and retried.
- The tap needs its own token (`HOMEBREW_TAP_GITHUB_TOKEN` or
  `repository.token`): `GITHUB_TOKEN` cannot write to another repository.

## GitHub Action

```yaml
- uses: magicdrive/hephaestus@<full-commit-sha>
  with:
    command: run
    stages: formula,tap
    github-token: ""
    tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

| Input | Default | Meaning |
|---|---|---|
| `command` | `run` | `run`, `check`, `build`, `archive`, `checksum`, `release`, `formula` or `tap` |
| `stages` | all configured | With `run`: comma-separated stages or `all` |
| `config` | `hephaestus.yaml` | Configuration file, relative to `working-directory` |
| `tag` | the pushed tag | Release tag |
| `snapshot` | `"false"` | Build without a tag |
| `dry-run` | `"false"` | Plan GitHub writes without performing them |
| `fresh` | `"false"` | Ignore an existing manifest |
| `working-directory` | `.` | Directory to run in |
| `github-token` | `${{ github.token }}` | Release token (`contents: write` to publish) |
| `tap-token` | `""` | Tap token (`contents: write`, `pull-requests: write` on the tap) |

| Output | Value |
|---|---|
| `status` | `published`, `no-op`, `dry-run` or `failed` |
| `no-op` | `"true"` when nothing changed on GitHub |
| `version` | Version being released |
| `artifacts` | JSON list of archives and checksum files (`name`, `path`, `sha256`, `url`) |
| `release-url` | URL of the GitHub Release |
| `release-id` | ID of the GitHub Release |
| `uploaded-assets` | JSON list of assets uploaded or replaced in this run |
| `formula-paths` | Newline-separated paths of generated formulae |
| `formula-commit-sha` | Newline-separated tap commit SHAs |
| `pull-request-url` | Newline-separated tap pull request URLs |

Permissions, token design, untrusted pull requests and complete workflows
(single job, build/publish split, GoReleaser, matrix):
[docs/github-action.md](docs/github-action.md).

## Security

- Sources are read through `os.Root`: no path, symbolic link or race can read
  outside the project root. Archives never contain link entries; unsafe
  modes, case-only collisions and credential-looking files are refused.
- Publishing tokens are removed from build commands' environment, hidden
  from templates (credential-like `.Env` names are hidden, and a rendered
  value containing a token is an error), sent only to their own origin over
  https, and replaced by `***` (also base64, URL-encoded and quoted forms) in
  errors, summaries and outputs. Build code runs as the same user and may
  still read the environment of the processes around it, so **build and
  publish in separate jobs** when you publish
  ([docs/github-action.md](docs/github-action.md#separate-build-and-publish-jobs)).
- Releases are draft-first and verified; taps are updated without force
  pushes.
- **Do not run publishing stages on `pull_request` or `pull_request_target`,
  and never give a fork's code your secrets.** Build in a job without write
  permissions and publish in a separate job, from tag pushes only. Pin the
  action to a commit SHA.

Threat model: [docs/security/threat-model.md](docs/security/threat-model.md).
Reporting vulnerabilities: [SECURITY.md](SECURITY.md).

## Limitations

- **No compilation of its own.** Cross-compiling is the build command's job;
  targets only parameterize it.
- **Archive formats**: `tar.gz` and `zip` only. No `tar.xz`, `tar.zst`,
  plain binaries as release assets per target (except `include: [binary]`
  with unique names), deb/rpm, Docker images, signing or SBOMs.
- **Publishing**: GitHub Releases and Homebrew taps on GitHub only. No GitLab
  or Gitea, no Homebrew casks, Scoop, winget or other package managers.
- **Homebrew**: only darwin/linux archives with `amd64` or `arm64`; 32-bit
  ARM, 386 and macOS universal binaries are skipped. One `def install` for
  every platform. Tap updates use the GitHub API (no plain git/SSH, no commit
  signing configuration).
- **Templates**: a small variable and function set (no dates, git metadata,
  file reads); see [GoReleaser compatibility](docs/compatibility/goreleaser.md#template-variables).
- **Release notes**: no changelog generation; use `notes`, `notes_file` or
  `generate_notes`.
- **Checksums**: SHA-256 only; archives only (binaries are not listed).
- **Paths**: Unicode normalization (NFC/NFD) collisions are not detected.

## Documentation

| Document | Contents |
|---|---|
| [CLI reference](docs/cli.md) | Every command, flag and exit code |
| [Configuration](docs/configuration.md) | Every field, default, constraint and template variable |
| [Archives](docs/archives.md) | File lists, path semantics, permissions, determinism |
| [GitHub Action](docs/github-action.md) | Inputs, outputs, permissions, tokens, workflows |
| [GoReleaser compatibility](docs/compatibility/goreleaser.md) | Field-by-field status of `brews`, archives, templates |
| [Migrating from GoReleaser](docs/migration/from-goreleaser.md) | Step-by-step migration |
| [Migration playbook](docs/development/hephaestus-migration.md) | Audit, compatibility, CI and first release when adopting Hephaestus in an existing project |
| [Architecture overview](docs/architecture/overview.md) | Stages, the Artifact Manifest, ownership, data flow |
| [Threat model](docs/security/threat-model.md) | Assets, attackers, defenses, residual risk |
| [Development](docs/development.md) | Make targets, tests, golden files, fuzzing, E2E |
| [Release preflight](docs/preflight.md) | `hephaestus preflight --tag vX.Y.Z`: read-only checks before pushing a tag |
| [Formula downgrade guard](docs/tap-version-guard.md) | Open formula pull requests of several releases, merge order, the tap's CI check, rollbacks |
| [Merge gate](docs/pr-gate.md) | `cmd/pr-gate`: fail-closed checks before merging a pull request, and a merge pinned to the reviewed head |
| [Recovering a release](docs/recovery.md) | `hephaestus status`, every failure point and its safe recovery |
| [Publishing a release](docs/publish.md) | `hephaestus publish vX.Y.Z` and `make publish vX.Y.Z` in your project: conventions, safety rules, recovery |
| [Releasing](docs/releasing.md) | How Hephaestus releases itself: `make publish vX.Y.Z`, prereleases, recovery |
| [Repository conventions](docs/architecture/repository-conventions.md) | Conventions shared with Ark and Enma |
| [Architecture](ARCHITECTURE.md) | Invariants for contributors |
| [Security policy](SECURITY.md) | Supported versions, reporting |

## Development

```sh
make build      # ./build/hephaestus
make test       # go test -count=1 ./...
make lint       # gofmt, vet, golangci-lint, tests, race, govulncheck
make snapshot   # Hephaestus packages itself and verifies the result
```

See [docs/development.md](docs/development.md) and
[CONTRIBUTING.md](CONTRIBUTING.md). AI coding agents: [AGENTS.md](AGENTS.md).

## Contributing

Bug reports and pull requests are welcome on
[GitHub](https://github.com/magicdrive/hephaestus/issues). Before changing a
stage, read [ARCHITECTURE.md](ARCHITECTURE.md): it lists the invariants a
plausible-looking change can break and the tests that enforce them.

## License

[MIT](LICENSE) © 2026 magicdrive (Hiroshi IKEGAMI)
