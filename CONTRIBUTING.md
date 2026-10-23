# Contributing to Hephaestus

Hephaestus aims to stay a small, predictable release tool. Bug fixes, tests
and documentation improvements are very welcome; for a new feature or a
change of behavior, please open an issue first so we can agree on it before
you write the code.

## Build and test

Requirements: Go (the version in `go.mod`), a POSIX shell, `tar` and `unzip`.
The full suite runs on Linux, macOS and Windows.

```bash
make build            # ./build/hephaestus
make test             # go test -count=1 ./...
make race             # go test -race (needs cgo)
make vet              # go vet for the host and for Windows
make fmt-check        # gofmt
make golangci-lint    # the pinned golangci-lint version CI uses
make vuln             # govulncheck
make lint             # all of the above
make fuzz             # every fuzz target briefly (FUZZTIME=30s for longer)
make snapshot         # Hephaestus packages itself; scripts/verify-dist.sh checks it
make e2e-homebrew     # real Homebrew end-to-end test (installs into your prefix)
make goreg            # order imports (make dev-tools installs goreg)
```

`-count=1` skips cached results. Tool versions in the Makefile equal the
ones in `.github/workflows/ci.yml`; a test keeps them equal.

CI runs lint, tests on Linux/macOS/Windows, race tests on the `go.mod` Go and
on stable Go, fuzz smoke tests, the self-snapshot, the Homebrew end-to-end
test on macOS and Linux, and a run of the action itself, behind one required
`ci-gate` job. The extended checks (`ci-extended.yml`: shuffled tests with
several seeds, repeated process tests, arm64 and Windows, longer fuzzing) run
weekly, by hand, and on a pull request labeled `ci-extended`.

Test layout, golden files and the E2E script: [docs/development.md](docs/development.md).

## Pull requests

- Keep a change focused; one topic per pull request.
- Add a test that fails without your change (a regression test for a bug
  fix).
- Read the section of [ARCHITECTURE.md](ARCHITECTURE.md) for the area you
  touch: it lists the invariants and the tests that pin them.
- When you change a configuration field, update
  [docs/configuration.md](docs/configuration.md) (and, for `brews`,
  [docs/compatibility/goreleaser.md](docs/compatibility/goreleaser.md)) in
  the same change. `repo_test.go` loads every configuration example in the
  documentation through the real loader and checks relative links and the
  action's inputs and outputs.
- After an intended change to formula output, rewrite the golden files with
  `go test ./internal/homebrew -update` and review the diff.
- Pin every new GitHub Action to a commit SHA with a `# vX.Y.Z` comment.
- Run `make lint` before pushing.
- Describe a user-visible change in the pull request: release notes are
  written from it.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`,
`docs:`, `test:`, `refactor:`, `chore:`, `ci:`, with an optional scope
(`fix(release): ...`). Dependency updates by Renovate use `chore(deps):`.

## AI coding agents

Agents should follow [AGENTS.md](AGENTS.md).

## Reporting bugs

Please include the Hephaestus version (`hephaestus version`, or the action's
commit), your OS or runner, the configuration (without secrets), the command
line, what you expected and what happened. Security issues: see
[SECURITY.md](SECURITY.md).
