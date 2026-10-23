# Repository conventions

Hephaestus is maintained alongside [Ark](https://github.com/magicdrive/ark)
and [Enma](https://github.com/magicdrive/enma). This document records which of
their repository layout, development conventions, quality gates and release
practices Hephaestus adopts, which it deliberately does not, and what it adds
for its own job — producing and publishing releases. Consistency with the
sibling projects is a goal; the right design for a supply-chain tool comes
first.

Reference snapshots: Ark at `50dc11e`, Enma at `4097a59` (2026-10-23).

## 1. Adopted from Ark

| Convention | In Ark | In Hephaestus |
| --- | --- | --- |
| Root `main.go` holds only `var version` (set with `-ldflags -X main.version=…`) and a `debug.ReadBuildInfo()` fallback, then calls the CLI package | `main.go` → `cmd/ark` | `main.go` → `cmd/hephaestus.Execute` |
| CLI in `cmd/<name>`, implementation in small `internal/*` packages with one responsibility each | `internal/{index,search,mcp,…}` | `internal/{config,archive,build,release,homebrew,tap,…}` |
| **Repository consistency tests at the root**: Go tests that fail when workflows, the Makefile or documentation drift from the code | `architecture_doc_test.go`, `docs_test.go`, `release_workflow_test.go` | `repo_test.go`: actions pinned to SHAs, read-only default permissions, release gated on the full CI, Makefile tool versions equal CI's, action inputs/outputs documented, relative links resolve, **every configuration example in the docs loads through the real config loader** |
| CI is reusable (`workflow_call`) and the release workflow runs it at the tagged commit before publishing | `release.yml` `uses: ./.github/workflows/ci.yml` | same, plus a single `ci-gate` job that requires every other job |
| Every third-party action pinned to a commit SHA with a `# vX.Y.Z` comment | yes | yes, enforced by `TestWorkflows_ActionsArePinnedToCommitSHAs` |
| Top-level `permissions: contents: read`; write scopes only on the publishing job | yes | yes, enforced by `TestWorkflows_DefaultPermissionsAreReadOnly` |
| Fuzz smoke in CI, failing inputs kept as artifacts | `fuzz-smoke` | `fuzz-smoke` runs every `Fuzz*` target via `scripts/fuzz.sh` (no hand-maintained list to drift) |
| ARCHITECTURE.md written as invariants plus "Verifying a change" and "Keeping this file true" | yes | yes |
| README structure: Why / How it works / Quick start / Capabilities / Security / Limitations / Documentation; README_ja.md | yes | yes |
| `goreg.toml` import order: std, third party, `github.com/magicdrive`, local | yes | identical file |
| Renovate: weekly, `chore(deps):`, `minimumReleaseAge: 3 days`, pinned action digests, OSV alerts, testdata and golden files ignored | yes | yes, without Ark's TypeScript-oracle rule |
| Errors wrapped with lowercase context, `fmt.Errorf("context: %w", err)`; diagnostics on stderr, no logging framework | yes | yes; validation errors additionally carry the YAML line and field path |
| Minimal dependencies, no CLI framework | 5 direct deps | 1 direct dependency (`gopkg.in/yaml.v3`) |
| Conventional Commits, work on feature branches merged by pull request | yes | yes |

## 2. Adopted from Enma

| Convention | In Enma | In Hephaestus |
| --- | --- | --- |
| Two-tier CI: a fast gate on every push and pull request, an extended run weekly, on demand, or with a `ci-extended` label | `ci.yml` / `ci-extended.yml` | same split; extended runs shuffled tests (three seeds), repeated process/timeout tests, arm64 and Windows, and every fuzz target for 60 s |
| Pinned tool versions run with `go run tool@version` | staticcheck, govulncheck, goreleaser | golangci-lint `v2.15.0`, govulncheck `v1.9.0`, declared once in `ci.yml` `env` and in the Makefile (kept equal by a test) |
| Race tests on the `go.mod` Go version and on stable Go | yes | `race` job matrix `[go.mod, stable]` × `[ubuntu, macos]` |
| Build a snapshot, verify the dist with a script, then publish | `scripts/verify-dist.sh` | `scripts/verify-dist.sh` checks archive contents, modes and checksums and runs the host binary; required before the release step (test-enforced) |
| A fixed checksum file name, `<project>_checksums.txt` | yes | yes |
| `CGO_ENABLED=0`, `-s -w -X main.version=v{{ .Version }}` | yes | yes |
| `.gitattributes` with `* text=auto eol=lf` | yes | yes, plus binary archives |
| End-to-end tests that drive the real binary against fixtures | `internal/e2e` | `cmd/hephaestus` CLI tests against temporary projects; `testdata/e2e/fixture` with `scripts/homebrew-e2e.sh` for real Homebrew |
| SECURITY.md (latest version supported, GitHub private advisories), CONTRIBUTING.md, pull request and issue templates | yes | yes |
| Makefile with `build`, `test`, `test-verbose`, `race`, `vet`, lint, `fuzz`, `dev-tools`, `goreg`, `help` | yes | yes, plus `golangci-lint`, `vuln`, `fmt-check`, `e2e-homebrew`, `snapshot` |

## 3. Not adopted, and why

| Element | Source | Reason |
| --- | --- | --- |
| `make publish tag=…` (tag and push from a developer machine) | Ark, Enma | Releases must come only from a tag push gated by CI. The Makefile never tags or pushes. |
| One personal access token for both the release and the tap | Ark, Enma (`PERSONAL_GITHUB_TOKEN`) | Least privilege: the release uses the workflow's `GITHUB_TOKEN`; only the tap step receives `HOMEBREW_TAP_GITHUB_TOKEN`, scoped to the tap repository, inside a `release` environment. |
| GoReleaser for our own releases (`version: '~> v1'` / deprecated `brews`) | Ark, Enma | Hephaestus releases itself with Hephaestus (`hephaestus.yaml`). Dogfooding exercises every stage on every release. |
| Per-platform `make` cross-builds | Ark | Duplicates the release configuration (and Ark's copy has an `LDFAGS` typo that drops the version). The build matrix lives in `hephaestus.yaml` only; `make snapshot` uses it. |
| Hand-maintained fuzz target list in the Makefile and CI | Ark | `scripts/fuzz.sh` discovers targets with `go test -list`, so a new fuzz test is covered automatically. |
| Python helpers in CI (`race-shard.py`, `check-coverage.py`) | Ark | Not needed at this size; checks are Go tests or shell. Race sharding can be added when the suite needs it. |
| staticcheck pinned to a pseudo-version | Ark | golangci-lint v2.15.0 (built with Go 1.27) runs staticcheck, gosec, errcheck and others in one configuration (`.golangci.yml`). |
| Planning documents at the repository root (`PLAN.md`, `ARK_*_PLAN.md`) | Ark | Design records live under `docs/`. |
| `install.sh` | Enma | Not yet: Hephaestus is installed through the GitHub Action, Homebrew or `go install`. The release layout (`<project>_checksums.txt`, `<project>_<version>_<os>_<arch>.tar.gz`) keeps Enma's installer contract so the script can be adopted later. |
| Ark's code-intelligence features, Enma's process supervision and its security mechanisms | Ark, Enma | Outside Hephaestus's responsibility. |
| `AGENTS.md` ignored as a per-developer file | Ark | Hephaestus commits a public AGENTS.md with build commands and package map for any AI agent; personal instructions stay in the git-ignored CLAUDE.md. |

## 4. Designed for Hephaestus

- **Artifact Manifest** (`dist/hephaestus-manifest.json`): the versioned, strictly validated, deterministically serialized contract between stages. Each stage owns specific kinds and fields and never invents data it did not observe.
- **Plan, then write**: archives, builds and preflight checks resolve everything first; nothing is written when any target fails.
- **Declarative archives** with exact path semantics, `os.Root` confinement and byte-reproducible output ([archives.md](../archives.md)).
- **Draft-first, idempotent publication** compatible with immutable releases; tap updates by pull request without force pushes.
- **Provenance**: release archives and the checksum file are attested with `actions/attest-build-provenance` (verify with `gh attestation verify`).
- **Real Homebrew in CI**: `brew audit --strict`, `brew install`, `brew test` on macOS and Linux for every change.
- **Composite GitHub Action that builds from its own pinned source**: no downloaded binary to trust; inputs are passed through `env`, never interpolated into scripts.

## 5. Repository layout

```text
.
├── main.go                    # version + cmd/hephaestus.Execute
├── repo_test.go               # repository consistency tests
├── action.yml                 # composite GitHub Action
├── hephaestus.yaml            # Hephaestus releases itself
├── cmd/hephaestus/            # CLI: commands, stage runner, outputs, summary
├── internal/
│   ├── config/                # schema, strict YAML loading, defaults, validation
│   ├── tmpl/                  # template rendering
│   ├── pathutil/ glob/        # path validation, glob matching and walking
│   ├── manifest/              # artifact manifest
│   ├── pipeline/              # stage order and preflight checks
│   ├── version/               # tag parsing
│   ├── build/                 # command, existing and GoReleaser providers
│   ├── archive/               # archive planning, writing, inspection
│   ├── checksum/              # checksum file
│   ├── github/                # GitHub REST client
│   ├── release/               # GitHub Release publisher
│   ├── homebrew/              # formula generation and verification
│   ├── tap/                   # tap publisher
│   ├── compat/goreleaser/     # .goreleaser.yaml brews adapter
│   └── ghaction/              # step outputs and summary
├── testdata/e2e/fixture/      # fixture CLI for end-to-end tests
├── scripts/                   # fuzz.sh, homebrew-e2e.sh, verify-dist.sh
├── docs/                      # user and design documentation
├── examples/                  # configurations for several languages
└── .github/workflows/         # ci.yml, ci-extended.yml, release.yml
```

Test fixtures live next to the code that uses them (`internal/*/testdata`),
golden files under `internal/homebrew/testdata/*.golden`.

## 6. CI/CD

| Workflow | Trigger | Jobs |
| --- | --- | --- |
| `ci.yml` | push to `main`, pull requests, manual, `workflow_call` | `lint` (gofmt, vet incl. Windows, `go mod tidy -diff`, golangci-lint, govulncheck, shellcheck) · `test` (ubuntu, macOS, Windows, coverage) · `race` (go.mod and stable Go) · `fuzz-smoke` · `snapshot` (self-package and verify) · `homebrew-e2e` (macOS, Linux) · `action-smoke` (runs `uses: ./`) · `ci-gate` |
| `ci-extended.yml` | weekly, manual, `ci-extended` label | shuffled tests on four platforms incl. arm64, repeated process tests, 60 s per fuzz target |
| `release.yml` | `v*` tag | `ci` (the whole CI at the tag) → `build` (`contents: read`): build Hephaestus, build/archive/checksum, `verify-dist.sh`, upload `dist/` → `release` (environment `release`): download and re-verify `dist/`, attest provenance, publish the release, generate the formula, open a tap pull request |

## 7. Test strategy

| Kind | Where |
| --- | --- |
| Unit | every package, `*_test.go` beside the code |
| Golden | `internal/homebrew/testdata/*.golden` (update with `go test ./internal/homebrew -update`) |
| Fuzz | glob, path validation, config parsing, manifest decoding, Ruby escaping, class names, install analysis — discovered by `scripts/fuzz.sh` |
| Integration | archives written and read back (Go readers and system `tar`/`unzip`), stateful fake GitHub servers for release and tap, CLI runs against temporary projects |
| End-to-end | `scripts/homebrew-e2e.sh`: build the fixture CLI for four targets, archive, check reproducibility, generate the formula, `brew audit --strict`/`install`/`test`, run the installed binary |
| Repository | `repo_test.go` |
| Race | `go test -race ./...` on Linux and macOS |

Reproducibility: tests never depend on wall-clock time, network or the host
Homebrew (except the opt-in E2E script), and use `t.TempDir()` for all files.

## 8. Release strategy

- Semantic versioning; tags `vMAJOR.MINOR.PATCH`, prereleases `-rc.N` are
  marked prerelease automatically.
- A tag push runs the complete CI at that commit; publication happens only
  after `ci-gate` succeeds and `verify-dist.sh` passes.
- The release is created as a draft, every asset is uploaded and verified,
  then it is published — safe with immutable releases and safe to re-run.
- Archives: `hephaestus_<version>_<os>_<arch>.tar.gz` (zip on Windows)
  containing `hephaestus`, `LICENSE`, `README.md`; `hephaestus_checksums.txt`.
- Homebrew: a pull request to `magicdrive/homebrew-tap`.
- Release notes: GitHub-generated notes unless `release.github.notes` is set.
- Details: [releasing.md](../releasing.md).
