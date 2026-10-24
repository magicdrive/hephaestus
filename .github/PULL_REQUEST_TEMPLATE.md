## What and why

<!-- What does this change, and which issue does it address? -->

## Checklist

- [ ] Tests added or updated (a regression test for a bug fix)
- [ ] `make lint` passes (gofmt, vet, golangci-lint, tests, race, govulncheck)
- [ ] ARCHITECTURE.md invariants still hold (or the change updates them and their tests)
- [ ] Configuration changes are reflected in `docs/configuration.md` (and `docs/compatibility/goreleaser.md` for `brews`)
- [ ] Formula golden files updated with `go test ./internal/homebrew -update` and the diff reviewed, if formula output changed
- [ ] New GitHub Actions are pinned to a commit SHA with a `# vX.Y.Z` comment
- [ ] A user-visible change is described above (for the release notes)
