# Hephaestus Architecture

Hephaestus turns build outputs into release artifacts and publishes them: it
builds or imports executables, packages them into reproducible archives,
writes a checksum file, publishes a GitHub Release and a Homebrew formula.
Each stage records what it produced in the Artifact Manifest, and later
stages trust nothing else.

Code tells WHAT. Tests prove BEHAVIOR. This file records WHY, and what must
not change: the invariants a plausible-looking "improvement" can silently
break. Every rule names its authority — the code or test that defines it. If
this file and its authority disagree, the authority wins and this file is the
bug.

Hephaestus's priority order is **correct before convenient**: a release tool
that publishes the wrong bytes, a half-finished release or a formula pointing
at the wrong file has done harm that a clear error would have avoided.

This file is for contributors. Users' documentation is in `docs/`
([configuration](docs/configuration.md), [archives](docs/archives.md),
[GitHub Action](docs/github-action.md)); the stage diagram and manifest
ownership table are in [docs/architecture/overview.md](docs/architecture/overview.md).

## 1. Stages and responsibility boundaries

```text
config ──► pipeline.Check ──► build ──► archive ──► checksum ──► release ──► formula ──► tap
(strict     (every input       (binaries) (archives) (sha256sum) (GitHub     (Homebrew   (tap PR or
 load)       before anything                                       Release)   formula)    commit)
             runs)
                        each stage reads and writes dist/hephaestus-manifest.json
```

| Package | Responsibility | Must not |
| --- | --- | --- |
| `internal/config` | Parse `hephaestus.yaml` strictly, apply defaults, validate everything with line numbers | Touch the file system beyond reading the configuration |
| `internal/pipeline` | Stage order; preflight check of every selected stage's inputs | Run anything |
| `internal/build` | Run commands per target, or import prebuilt files / GoReleaser output; record binaries | Cross-compile, guess targets, trust another tool's checksum |
| `internal/archive` | Plan exact archive contents; write byte-reproducible tar.gz/zip; inspect untrusted archives | Read outside the project root; add files the configuration did not declare |
| `internal/checksum` | Write the checksums file from re-hashed archives | Copy a digest without re-deriving it |
| `internal/release` | Draft-first, idempotent GitHub Release publication | Publish before every asset is verified |
| `internal/homebrew` | Generate and verify formulae from the manifest | Emit unescaped data into Ruby |
| `internal/tap` | Publish formulae by pull request or direct commit | Move or force-update a reference |
| `internal/github` | Minimal REST client: retries, redirects, pagination, limits | Send a token to another origin; retry a POST blindly |
| `internal/manifest` | The stage contract: schema, validation, canonical serialization | Accept unknown fields |
| `internal/tmpl` | Strict template rendering with a small function set | Offer side effects (files, time, network) |
| `internal/pathutil`, `internal/glob` | Lexical path validation; glob matching and walking | Touch the file system (pathutil) |
| `internal/compat/goreleaser` | Classify GoReleaser `brews` keys | Silently drop a key |
| `internal/ghaction` | Step outputs and summary | Let a value inject another output |
| `internal/publish` | Release Hephaestus itself (`make publish`): preflight, annotated tag push, follow and verify the release run | Publish, upload or write to GitHub; move, delete or force-push a tag |
| `cmd/hephaestus` | Flags, stage loop, report | Contain stage logic |

## 2. Invariants

Each is enforced by tests; the "danger" line is the change that looks like an
improvement and is not.

1. **Plan, then write.** Every input of every selected stage is checked
   before the first stage runs; a command build renders and validates every
   target before running any; an archive stage plans every archive before
   writing one; the release stage verifies every asset locally before calling
   GitHub.
   Authority: `pipeline.Check`, `build.runCommand`, `archive.BuildPlans`,
   `release.prepare`.
   Tests: `TestCheckMissingInputs`, `TestPreflightErrors`,
   `TestInvalidOutputs`, `TestPreflightValidation`.
   Danger: "fail fast" by starting work and checking the rest later.

2. **The manifest never holds data that was not observed.** Sizes and
   SHA-256 are computed from the bytes on disk; a checksum claimed by another
   tool is only compared; a file that changes between planning and writing,
   or after it was recorded, is an error; `download_url` is recorded only
   after an upload was verified on a published release.
   Authority: `internal/manifest` package comment, `build.openVerified`,
   `checksum.Run`, `release.verifyFile`, `release.recordURLs`.
   Tests: `TestSourceChangedBetweenPlanAndWrite`, `TestTamperDetected`,
   `TestFileChangedAfterVerificationIsNotUploadedSilently`,
   `TestGoReleaserRejects`, `TestDraftConfigKeepsDraftAndSkipsURLs`,
   `TestTamperedFormula`.
   Danger: copying `artifacts.json` checksums, or filling a URL from a
   naming convention, "to save a step".

3. **Each stage owns its kinds and fields.** A stage adds only its own
   artifact kind (or fills the fields it owns) and, on a re-run, replaces
   only what it produced itself.
   Authority: `docs/architecture/overview.md` (ownership table),
   `archive.Run`, `build.record`.
   Tests: `TestRerunReplacesManifestEntries` (build and archive),
   `TestRerunKeepsImportedArchives`, `TestBinaryNameClashAcrossBuilds`.
   Danger: a stage "cleaning up" artifacts of another kind.

4. **Draft first; publish last.** A new release is always created as a draft
   and published only when every asset is present and verified. A failure
   leaves a draft and an error; the next run resumes. A lost create is
   followed by a lookup, never by a second POST. A published immutable
   release is never modified.
   Authority: `internal/release` package comment.
   Tests: `TestCreateUploadPublish`, `TestPartialFailureDoesNotPublish`,
   `TestCreateFailureDoesNotDuplicateRelease`,
   `TestUploadLostResponseIsVerifiedNotRepeated`,
   `TestImmutablePublishedRelease`.
   Danger: creating the release published "because draft: false".

5. **Re-running is safe and quiet.** Identical assets and identical formulae
   are no-ops: no upload, no commit, no pull request.
   Tests: `TestRerunIsNoOp`, `TestDirectUpdateAndNoOp`, `TestUnchangedOnBase`,
   `TestPullRequestNewFileAndRerunReuses`.
   Danger: "always re-upload to be sure".

6. **No force push, ever.** Git references are only created, never moved.
   Commits use the contents API with the blob SHA as an optimistic lock.
   Authority: `internal/tap` package comment.
   Tests: `TestNoRefUpdateAPI`, `TestDirectConflictRecovery`,
   `TestCreateRefRaceAccepted`.
   Danger: resetting the head branch "to start clean".

7. **Reads are confined to the project root.** Sources, build outputs and
   prebuilt files are opened through `os.Root`; every configured or rendered
   path is validated lexically first; symbolic links resolving outside the
   root are refused; archives contain no link entries.
   Authority: `internal/pathutil`, `internal/archive/plan.go` comment,
   `build.openVerified`.
   Tests: `TestValidateRelative`, `FuzzValidateRelative`, `TestInvalidPaths`,
   `TestSymlinks`, `TestOutputSymlinks`, `TestDirMustBeInsideRoot`,
   `TestRunRefusesSymlinkEscape`.
   Danger: `os.Open(filepath.Join(root, p))`.

8. **Untrusted archives are inspected, never extracted.** Imported archives
   are rejected for traversal, links, special files, unsafe modes, collisions
   and size bombs.
   Authority: `archive.Inspect`.
   Tests: `TestInspectRejectsMaliciousTar`, `TestInspectRejectsMaliciousZip`,
   `TestInspectRejectsGarbage`, `TestExistingArchiveRejected`.

9. **Same inputs, same bytes.** Archive entry order, times, modes, ownership
   and compression headers are fixed; manifests serialize canonically.
   Authority: `docs/archives.md` ("Deterministic output").
   Tests: `TestDeterministicOutput`, `TestMTimeSources`,
   `TestRoundTripAndDeterminism`.
   Danger: using the file system's mtime or walk order.

10. **Data in Ruby is always a literal.** Configuration values that become
    Ruby data are escaped string literals; user Ruby code is checked so it
    cannot escape its block, and template values substituted into it are
    restricted to a safe alphabet.
    Authority: `homebrew.RubyString`, `homebrew.codeBlock`,
    `homebrew.renderCode`.
    Tests: `TestRubyString`, `FuzzRubyString`, `FuzzRubyHeredoc`,
    `TestRubyInjection`, `TestRubyInjectionInCodeFields`,
    `TestRenderCodeValidatesValues`.

11. **Tokens go only where they are needed.** A token is sent only to the
    origin of the request (redirects to other hosts drop it, https→http is
    refused); it never appears in an error; the release token and the tap
    token are separate; build commands do not inherit publishing tokens.
    Authority: `internal/github` package comment, `internal/build`.
    Tests: `TestRedirectToOtherHostDropsAuthorization`,
    `TestRedirectHTTPSToHTTPRefused`, `TestTokenNeverInErrors`,
    `TestTokenNotInErrors`, `TestPermissionErrorHintsAndHidesToken`.
    Danger: one token "for simplicity".

12. **POST is never retried automatically.** Creating a release, an asset, a
    reference or a pull request may have succeeded when the response was
    lost; callers re-read state first.
    Tests: `TestPOSTIsNeverRetried`, `TestCreatePull422ReSearches`,
    `TestRecoverFromUpload502LeavingStarter`.

13. **Configuration is strict and fully checked up front.** Unknown and
    duplicate keys, aliases, invalid templates (every template is
    test-rendered) are errors with a line and path; warnings never fail a
    run.
    Tests: `TestUnknownFieldsReportLine`, `TestValidationErrorsAreLocated`,
    `TestRejects`, `FuzzParse`.

14. **Documentation is tested.** Every Hephaestus configuration example in
    the Markdown files loads through `config.Load`; relative links resolve;
    every action input and output is documented; workflows pin actions and
    gate publication on the full CI and `verify-dist.sh`.
    Authority: `repo_test.go`.

15. **A tag push is the only release trigger, and a pushed tag is final.**
    `make publish` pushes one annotated tag, never forced, only after a
    preflight that requires the exact commit's CI to be green, and then only
    observes: the release workflow is the sole publisher. A version is a
    prerelease exactly when `version.Info.IsPrerelease` says so; the release
    stage, `skip_upload: auto` and the workflow's tap steps (through
    `publish plan`) all use it, and a prerelease's tap steps get no tap
    token.
    Authority: `internal/publish` (`Publish`, `Preflight`), `release.yml`.
    Tests: `TestPublishRefusals`, `TestGitCLI`, `TestGHCLIIsReadOnly`,
    `TestReleaseWorkflow_TapTokenOnlyWhenTapPublished`,
    `TestPrereleaseTapNeedsNoToken`, `TestMakefile_Publish`.
    Danger: "just re-tag it" after a failed release.

## 3. Verifying a change

CI (`.github/workflows/ci.yml`) is the quality contract; `make lint` runs
most of it locally: gofmt, `go vet` for the host and Windows, golangci-lint,
`go test`, `go test -race`, govulncheck. In addition:

- `go test ./internal/homebrew -update` rewrites the formula golden files in
  `internal/homebrew/testdata/`; every diff is a behavior change to justify.
- `make fuzz` runs every `Fuzz*` target briefly (`scripts/fuzz.sh`).
- `make snapshot` builds, archives and checksums Hephaestus itself and runs
  `scripts/verify-dist.sh`.
- `make e2e-homebrew` runs the real Homebrew (`brew audit --strict`,
  `install`, `test`) against the fixture in `testdata/e2e/fixture`; CI runs it
  on macOS and Linux.
- A change to the manifest schema bumps `manifest.SchemaVersion`.
- A change to a configuration field updates `docs/configuration.md` (and,
  for `brews`, `docs/compatibility/goreleaser.md`) in the same change.

Details: [docs/development.md](docs/development.md).

## 4. Keeping this file true

- Write here only invariants, reasons and boundaries. Not counts, test-case
  lists, roadmaps or history — they rot, and git keeps history.
- Changing an invariant on purpose: change its authority test and this file
  in the same change, and say so in the commit message.
- When a test named here is renamed, update this file; a stale name is a
  bug in this file.
