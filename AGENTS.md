# AGENTS.md

Guidance for AI coding agents working on Hephaestus. Humans: see
[CONTRIBUTING.md](CONTRIBUTING.md).

## Build and test

```bash
go build ./...                       # or: make build (writes ./build/hephaestus)
go test -count=1 ./...               # all tests (make test)
go test -count=1 .                   # repository consistency tests (repo_test.go)
go test -race -count=1 ./...         # make race (needs cgo)
go vet ./... && GOOS=windows go vet ./...
gofmt -l .                           # must print nothing
go run . check --config <file>       # validate a configuration without running it
```

`make lint` runs every local gate (gofmt, vet, golangci-lint, tests, race,
govulncheck). Formula golden files: `go test ./internal/homebrew -update`,
then review the diff. Fuzz targets: `make fuzz`.

## Package map

| Path | Responsibility |
| --- | --- |
| `main.go` | Version (`-X main.version`) and `cmd/hephaestus.Execute` |
| `cmd/hephaestus` | Flags, stage loop, report, action outputs and summary |
| `internal/config` | `hephaestus.yaml` schema (`types.go`, `brew.go`), strict loading (`load.go`), defaults and validation (`validate.go`) |
| `internal/tmpl` | Template variables and functions |
| `internal/pipeline` | Stage order and preflight checks |
| `internal/manifest` | Artifact Manifest: the contract between stages |
| `internal/build` | `command`, `existing` and `goreleaser` build providers |
| `internal/archive` | Archive planning, writing and inspection of untrusted archives |
| `internal/checksum` | Checksum file |
| `internal/release` | GitHub Release publisher |
| `internal/homebrew` | Formula generation and install-path verification |
| `internal/tap` | Tap publisher (pull request and direct modes) |
| `internal/github` | GitHub REST client |
| `internal/compat/goreleaser` | GoReleaser `brews` adapter |
| `internal/pathutil`, `internal/glob` | Path validation, glob matching and walking |
| `internal/ghaction` | `$GITHUB_OUTPUT` and step summary |
| `internal/version` | Tag parsing; the one definition of "prerelease" |
| `internal/preflight` | `hephaestus preflight`: read-only release checks (git, GitHub settings, config, tap) with stable check IDs |
| `internal/workflowcheck` | the release workflow conventions (`workflow.*` preflight checks, `check --workflows`), read from the YAML only |
| `cmd/hephaestus/init.go` | `hephaestus init`: starting configurations (go, rust, script, prebuilt) |
| `internal/status` | `hephaestus status`: read-only release state and next safe action |
| `internal/publish`, `internal/publish/cli` | `hephaestus publish` (preflight, tag push, run monitoring, verification) for any project following docs/publish.md |
| `internal/cmd/publish` | This repository's release tool behind `make publish`: `internal/publish/cli` with Hephaestus's own help, built from the tagged source, plus the `action/VERSION` check |
| `repo_test.go` | Workflows, Makefile, action and documentation consistency |
| `action.yml` | Composite GitHub Action |
| `testdata/e2e/fixture` | Fixture CLI for `scripts/homebrew-e2e.sh` |

## Rules

- Read [ARCHITECTURE.md](ARCHITECTURE.md) before changing a stage; keep its
  invariants (plan before write, never invent manifest data, draft-first,
  no force push, reads confined with `os.Root`, tokens only where needed).
- Read the package comment of the code you edit; many are normative.
- Configuration changes: update `internal/config`, `docs/configuration.md`
  and, for `brews`, `docs/compatibility/goreleaser.md` together. Every
  `yaml` code block whose top-level keys are Hephaestus keys is loaded by
  `repo_test.go`; keep examples valid.
- Every `action.yml` input and output must appear in backticks in
  `README.md` or `docs/github-action.md`.
- Errors: lowercase, wrapped with context (`fmt.Errorf("context: %w", err)`);
  validation errors carry the YAML line and field path. No logging framework.
- Tests: no network, no wall-clock dependence, files under `t.TempDir()`;
  GitHub interactions use the fake servers in `internal/release` and
  `internal/tap` tests.
- Imports: std, third party, `github.com/magicdrive`, local (`goreg.toml`).
- Commits: Conventional Commits. Do not tag or publish releases; releases
  are cut by pushing a tag (see [docs/releasing.md](docs/releasing.md)).
