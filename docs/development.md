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
| `make test-offline` | `go test -count=1 ./...` with no network (Linux, sudo; `scripts/test-offline.sh`) |
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
| `make dev-tools` | Install `goreg` at `GOREG_VERSION` |
| `make renovate-check` | Validate `renovate.json` with the Renovate pinned by `.github/renovate-check/package-lock.json` (Node.js of `.github/renovate-check/.node-version`, `npm ci`) |
| `make renovate-check-test` | Failure-injection tests of that pinned install against a fake registry (Python 3, Node.js) |
| `make goreg` | Order imports (std, third party, `github.com/magicdrive`, local) |
| `make publish vX.Y.Z` | Release Hephaestus ([releasing.md](releasing.md)); also `publish-check`, `publish-status`, `publish-watch`, `publish-help` |

`GOLANGCI_LINT_VERSION` and `GOVULNCHECK_VERSION` in the Makefile must equal
the values in `.github/workflows/ci.yml`; `TestMakefile_ToolVersionsMatchCI`
enforces it.

## Tests

| Kind | Where | Notes |
| --- | --- | --- |
| Unit | `*_test.go` beside the code | No network (enforced: `scripts/test-offline.sh`, CI job `test-offline`), no wall-clock dependence, files under `t.TempDir()` |
| Golden | `internal/homebrew/testdata/*.golden` | Generated formulae; also parsed with `ruby -c` when Ruby is installed (`TestGoldenRubySyntax`) |
| Integration | `internal/archive` (archives written and read back with Go and with system `tar`/`unzip`), `internal/release` and `internal/tap` (stateful fake GitHub servers), `cmd/hephaestus` (CLI runs against temporary projects) | |
| Fuzz | `Fuzz*` functions in `config`, `manifest`, `pathutil`, `glob`, `homebrew` | Discovered automatically by `scripts/fuzz.sh` |
| Repository | `repo_test.go` | Pinned actions, read-only default permissions, release gating, Makefile/CI tool versions, documented action inputs and outputs, relative links, configuration examples in Markdown |
| End-to-end | `scripts/homebrew-e2e.sh`, fixture in `testdata/e2e/fixture` | Real `brew` |

### No network in unit and integration tests

`go test ./...` must pass without any network: fake GitHub servers listen
on loopback, and nothing reaches the module proxy or downloads a toolchain.
`scripts/test-offline.sh` (`make test-offline`, Linux, needs sudo) downloads
the modules first, then runs `go test -count=1 ./...` in a new network
namespace with loopback only, `GOPROXY=off`, `GOFLAGS=-mod=mod` and
`GOTOOLCHAIN=local`; it fails if the namespace can reach an outside address
or resolve a name. The CI job `test-offline` runs it on every change. Tests
that check public GitHub (published releases, attestations, the production
tap) are separate end-to-end jobs; see [Network use in CI](#network-use-in-ci).

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
gate): lint, tests on three OSes, tests without network, race tests, fuzz smoke, snapshot,
Homebrew E2E, a run of the action itself, and `ci-gate`, the single required
check. `.github/workflows/ci-extended.yml` (weekly, manual, `ci-extended`
label): shuffled tests on four platforms, repeated process tests, 60 s per
fuzz target.

### Network use in CI

Every job checks out the repository and (except the jobs that run the
action without Go) sets up Go with `actions/setup-go`, which downloads the
toolchain unless the runner image has it, and restores and saves the module
cache through the Actions cache service. Beyond that:

| Job | Network beyond checkout and setup-go | Needed |
| --- | --- | --- |
| `lint` | Modules of golangci-lint and govulncheck (`go run pkg@version`, module proxy and checksum database); vulnerability database (vuln.go.dev) | Yes: the tools and the database are the check |
| `renovate-check` | Node.js (`setup-node`, `.node-version`); Renovate and its dependencies from the npm registry by the lockfile (`npm ci`, integrity checked); the tests use a fake registry on loopback | Yes: the validator is the check |
| `test` | Modules for the build (module proxy); the tests themselves none | Modules only; removable with a module cache |
| `test-offline` | Modules before the tests; the tests run in a namespace with loopback only | Modules only |
| `completion` | `apt-get` (zsh, fish) on Linux, `brew install fish` on macOS | Yes: real shells |
| `race` | `stable` Go for one matrix leg | Yes: tests the next Go |
| `fuzz-smoke`, `snapshot` | Modules; artifact upload on fuzz failure | Modules only |
| `homebrew-e2e` | Homebrew (`brew audit --strict` and `brew install` of a local tap; `brew install fish`, `apt-get`); the v1.0.0 formula of the production tap at a fixed commit and its release archive | Yes: real Homebrew and the upgrade from a published version |
| `action-binary` | Modules; artifact upload | Artifact service only |
| `action-install` | None | — |
| `e2e-action-install` | Published v1.1.0 and v1.2.0-rc.1 release assets, `gh attestation verify` (GitHub API, Sigstore trust root) | Yes: the real release path |
| `action-release` | The published action v1.2.0-rc.1, its release binary and attestation | Yes: the action as callers run it |
| `action-smoke`, `action-boundary`, `action-isolation` | Artifact download of this commit's binaries; `setup-go` of the caller's Go for the fixture (no cache) | Artifact service and the caller's Go |
| `action-isolation-detects-v1-1-0` | The published action v1.1.0, which installs Go and compiles Hephaestus | Yes: shows the isolation check can fail |
| `ci-gate` | None | — |

### Pinned and floating dependencies

Everything CI and the Makefile fetch is pinned: actions to commit SHAs
(`TestWorkflows_ActionsArePinnedToCommitSHAs`), Go tools to versions
(`go run`/`go install pkg@vX.Y.Z`; the module proxy and checksum database
verify them), the Go toolchain by `go.mod` (`toolchain`), Renovate and its
whole tree by `.github/renovate-check/package-lock.json`, Node.js by
`.github/renovate-check/.node-version`. Renovate updates all of them,
the Makefile/`ci.yml` tool versions through `customManagers`.
`TestFloatingDependencies` fails on `@latest`, `npx`, `npm install` and
`go-version: stable` outside the exceptions below.

These float on purpose:

| What | Where | Why |
| --- | --- | --- |
| `go-version: stable` | `ci.yml` `race` (one leg) | Tests the newest Go release before `go.mod` moves to it |
| `go-version: "1.26"` / `"1.27"` (latest patch) | `ci.yml` `action-isolation`, `action-isolation-detects-v1-1-0` | The caller's Go as callers install it; the jobs compare it before and after the action |
| `go-version: stable` | `examples/workflows/tap-version-guard.yml` | A tap's CI compiles the guard with a current Go; the guard itself is pinned to a commit |
| Runner images `ubuntu-latest`, `macos-latest`, `windows-latest` | Every workflow | GitHub's maintained images; the matrix also names fixed images (`ubuntu-24.04-arm`, `macos-15-intel`, `windows-11-arm`). Release builds do not depend on the image: Go comes from `go.mod` and builds are `-trimpath` |
| `apt-get install zsh fish`, `brew install fish`, Homebrew itself | `completion`, `homebrew-e2e` | Real shells and the current Homebrew are what the tests check |

There are no container images and no `npx` in this repository.

The release workflows (`release.yml`, `release-rehearsal.yml`) need GitHub
(release, attestations, the tap) by design; `verify-release.yml` downloads a
published release.

## Releasing

See [releasing.md](releasing.md).
