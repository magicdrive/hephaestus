# Hephaestus

**Forge release artifacts into reliable distributions.**

[日本語](README_ja.md) · [Configuration](docs/configuration.md) ·
[Archives](docs/archives.md) · [GitHub Action](docs/github-action.md) ·
[GoReleaser compatibility](docs/compatibility/goreleaser.md) ·
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
| Over-shared tokens | Release and tap tokens are separate; build commands never inherit them |

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

**2. Configure** — `hephaestus.yaml` at the repository root:

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

```sh
go install github.com/magicdrive/hephaestus@latest   # Go 1.26 or later
```

After the first release, also:

```sh
brew install magicdrive/tap/hephaestus
```

or download an archive from the GitHub Releases page; release archives and
the checksum file carry build provenance attestations
(`gh attestation verify <file> --repo magicdrive/hephaestus`).

In GitHub Actions, use the action (it builds Hephaestus from the commit you
pin):

```yaml
- uses: magicdrive/hephaestus@<full-commit-sha>
```

## Usage

```text
hephaestus check              validate the configuration and print the plan
hephaestus run                run every configured stage (or --stages a,b)
hephaestus build|archive|checksum|release|formula|tap   run one stage
hephaestus version
```

| Flag | Meaning |
|---|---|
| `--config PATH` | Configuration file (default: `hephaestus.yaml` in the current directory). |
| `--tag TAG` | Release tag (default: `$GITHUB_REF_NAME` on tag pushes). Required unless `--snapshot`. |
| `--snapshot` | Version `0.0.0-SNAPSHOT`, no tag; the release and tap stages refuse to run. |
| `--stages LIST` | `run` only: comma-separated stages or `all`. |
| `--dry-run` | Plan release and tap writes without performing them. |
| `--fresh` | Start a new manifest instead of continuing the existing one. |
| `--commit SHA` | Commit (default: `$GITHUB_SHA`). |
| `--repo OWNER/NAME` | Release repository (default: configuration, then `$GITHUB_REPOSITORY`). |
| `--token-env NAME`, `--tap-token-env NAME` | Variables holding the tokens (defaults `GITHUB_TOKEN`, `HOMEBREW_TAP_GITHUB_TOKEN`). |

Each run prints the SHA-256 of every archive, checksum file and formula and
ends with `==> published`, `no-op`, `dry-run` or `failed`.

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
  `replace_existing_assets: true`. A lost response is followed by a lookup,
  never by a duplicate create.
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
- Publishing tokens are never passed to build commands, never interpolated
  into templates by accident (credential-like `.Env` names are hidden), sent
  only to their own origin over https, and replaced by `***` in errors,
  summaries and outputs.
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
| [Configuration](docs/configuration.md) | Every field, default, constraint and template variable |
| [Archives](docs/archives.md) | File lists, path semantics, permissions, determinism |
| [GitHub Action](docs/github-action.md) | Inputs, outputs, permissions, tokens, workflows |
| [GoReleaser compatibility](docs/compatibility/goreleaser.md) | Field-by-field status of `brews`, archives, templates |
| [Migrating from GoReleaser](docs/migration/from-goreleaser.md) | Step-by-step migration |
| [Architecture overview](docs/architecture/overview.md) | Stages, the Artifact Manifest, ownership, data flow |
| [Threat model](docs/security/threat-model.md) | Assets, attackers, defenses, residual risk |
| [Development](docs/development.md) | Make targets, tests, golden files, fuzzing, E2E |
| [Releasing](docs/releasing.md) | How Hephaestus releases itself |
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
