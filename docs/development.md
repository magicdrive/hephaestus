# Development

Requirements: Go (the version in `go.mod`), a POSIX shell, `tar` and `unzip`.
Homebrew only for the end-to-end test.

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | Build `./build/hephaestus` with the version from `git describe` |
| `make install` | `go install` with version information |
| `make test` | `go test -count=1 ./...` |
| `make test-verbose` | Clean the test cache, run tests verbosely |
| `make race` | `go test -race -count=1 ./...` (needs cgo) |
| `make vet` | `go vet` for the host and for `GOOS=windows` |
| `make fmt-check` | Fail when `gofmt -l .` lists files |
| `make golangci-lint` | golangci-lint at the version CI pins (`.golangci.yml`) |
| `make vuln` | govulncheck at the version CI pins |
| `make lint` | `fmt-check`, `vet`, `golangci-lint`, `test`, `race`, `vuln` |
| `make fuzz` | Every fuzz target for `FUZZTIME` (default `10s`) via `scripts/fuzz.sh` |
| `make e2e-homebrew` | Real Homebrew end-to-end test (`scripts/homebrew-e2e.sh`) |
| `make snapshot` | Build, archive and checksum Hephaestus with itself; verify with `scripts/verify-dist.sh` |
| `make clean` | Remove `./build` |
| `make dev-tools` | Install `goreg` |
| `make goreg` | Order imports (std, third party, `github.com/magicdrive`, local) |
| `make publish vX.Y.Z` | Release Hephaestus ([releasing.md](releasing.md)); also `publish-check`, `publish-status`, `publish-watch`, `publish-help` |

`GOLANGCI_LINT_VERSION` and `GOVULNCHECK_VERSION` in the Makefile must equal
the values in `.github/workflows/ci.yml`; `TestMakefile_ToolVersionsMatchCI`
enforces it.

## Tests

| Kind | Where | Notes |
| --- | --- | --- |
| Unit | `*_test.go` beside the code | No network, no wall-clock dependence, files under `t.TempDir()` |
| Golden | `internal/homebrew/testdata/*.golden` | Generated formulae; also parsed with `ruby -c` when Ruby is installed (`TestGoldenRubySyntax`) |
| Integration | `internal/archive` (archives written and read back with Go and with system `tar`/`unzip`), `internal/release` and `internal/tap` (stateful fake GitHub servers), `cmd/hephaestus` (CLI runs against temporary projects) | |
| Fuzz | `Fuzz*` functions in `config`, `manifest`, `pathutil`, `glob`, `homebrew` | Discovered automatically by `scripts/fuzz.sh` |
| Repository | `repo_test.go` | Pinned actions, read-only default permissions, release gating, Makefile/CI tool versions, documented action inputs and outputs, relative links, configuration examples in Markdown |
| End-to-end | `scripts/homebrew-e2e.sh`, fixture in `testdata/e2e/fixture` | Real `brew` |

### Golden files

After an intended change to formula output:

```sh
go test ./internal/homebrew -run TestGolden -update
git diff internal/homebrew/testdata/
```

Every diff is a behavior change to justify in the pull request.

### Fuzzing

```sh
make fuzz                         # every target for 10s
FUZZTIME=60s make fuzz            # longer
go test ./internal/pathutil -run '^$' -fuzz '^FuzzValidateRelative$' -fuzztime 5m
```

A failing input is written to `testdata/fuzz/<FuzzName>/` in the package;
commit it with the fix so it stays a regression test.

### Documentation tests

`go test -count=1 .` runs `repo_test.go`. It loads every `yaml` code block in
`*.md`, `docs/*.md`, `docs/*/*.md`, `examples/*.md` and `examples/*/*.md`
whose top-level keys are all Hephaestus keys (`version`, `project`, `builds`,
`archives`, `checksum`, `release`, `brews`, `homebrew`, `limits`, `env`,
`dist`, `manifest`) through `config.Load` (a missing `project` is added).
Workflow files, GoReleaser excerpts (with `project_name`) and manifests are
skipped. Use another fence language for an intentionally invalid example.
Examples under `examples/*/hephaestus.yaml` are checked with:

```sh
for f in examples/*/hephaestus.yaml; do go run . check --config "$f"; done
```

### End-to-end test

`make e2e-homebrew` builds the fixture CLI for four targets with Hephaestus,
archives it, checks the archive layout and byte-for-byte reproducibility,
generates a formula with `file://` URLs (`HEPHAESTUS_ALLOW_FILE_URLS=1`), and
runs `brew audit --strict`, `brew install`, `brew test` and `brew uninstall`
in a throw-away tap. It installs into your Homebrew prefix and removes what
it created on exit; CI runs it on macOS and Linux.

## CI

`.github/workflows/ci.yml` (push to `main`, pull requests, and as the release
gate): lint, tests on three OSes, race tests, fuzz smoke, snapshot,
Homebrew E2E, a run of the action itself, and `ci-gate`, the single required
check. `.github/workflows/ci-extended.yml` (weekly, manual, `ci-extended`
label): shuffled tests on four platforms, repeated process tests, 60 s per
fuzz target.

## Releasing

See [releasing.md](releasing.md).
