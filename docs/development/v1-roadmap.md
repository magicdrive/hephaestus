# Road to v1.0.0

Baseline audit of v0.2.0 and the plan to a v1.0.0 release candidate.
Progress is tracked in [v1-progress.md](v1-progress.md).

## Baseline (v0.2.0)

| Area | What exists |
| --- | --- |
| Pipeline | `build` (Go and command builds, GoReleaser `artifacts.json` import), `archive` (tar.gz/zip, deterministic, path confinement), `checksum`, `release` (draft-first, idempotent, immutable-aware, digest-verified), `formula` (multi-platform, GoReleaser `brews` compatible), `tap` (pull request or direct, no force-push) |
| Safety | strict config (unknown keys rejected), `os.Root` confinement, token redaction, `hephaestus preflight` (read-only, 25 checks, JSON, strict, exit codes), `make publish` orchestrator for Hephaestus itself (29 checks, confirm, tag, follow, verify) |
| Distribution | composite GitHub Action (`action.yml`), release workflow with provenance attestation, `release` environment, tag rulesets, immutable releases |
| Tests | unit tests per package, fake GitHub server, fuzzing, `brew style`/`audit`/`install` E2E on macOS and Linux, Action smoke and boundary jobs on 3 OSes |

## Gaps found

| # | Gap | Area | Priority |
| --- | --- | --- | --- |
| 1 | The Action cannot run `preflight` | Release Safety | P0 |
| 2 | Release notes / changelog not checked against the tag (v0.2.0 notes once named v0.1.0) | Release Safety | P0 |
| 3 | `make publish` checks and the generic preflight are separate; the generic one is not a gate | Release Safety | P0 |
| 4 | No noarch archives: script CLIs (magicdrive/t) need one archive for all platforms | Artifact | P1 |
| 5 | Formula installs that move one file twice pass generation and fail in `brew install` (t's first formula) | Formula | P1 |
| 6 | No read-only way to see where a half-done release stands; recovery is described for the release stage only | Recovery | P0 |
| 7 | Untested recovery paths: publish failure, lost publish response, CLI resume after a stage failure, immutable metadata update | Recovery | P0 |
| 8 | Linux arm64 only in the weekly CI; macOS Intel never | CI | P2 |
| 9 | No `init`/templates: a new project starts from the docs | DX | P2 |
| 10 | No compatibility policy or stable surface definition; no regression fixtures of real v0.2.0 configs | Compatibility | P1 |

Technical debt: `internal/publish` duplicates some GitHub reads of
`internal/preflight` (kept: publish needs Hephaestus-specific checks);
`cmd/hephaestus/run.go` is long; no machine-readable config schema.

Compatibility risks: new preflight checks can turn a passing release into a
failing one; a formula layout change can produce `brew style` offenses in a
tap. Security risks: tokens reaching annotations or step summaries (now
redacted), tap PAT scope (rehearsed read-only before publishing).

## Phases and dependencies

```
Phase 1 Release Safety ─┬─> Phase 3 Recovery ──> Phase 4 CI & Security ─┐
Phase 2 Artifact/Formula ┘                                              ├─> Phase 7 Dogfooding ─> Phase 8 Stabilization
                         Phase 5 DX ── Phase 6 Compatibility ───────────┘
```

Pull requests are stacked in this order: release safety, noarch and formula
quality, recovery, stable contract (docs), E2E and security, developer
experience, stabilization.

## v1.0.0 Release Gate

A release candidate is ready only when every gate is GREEN with evidence.
UNKNOWN is never PASS.

| Gate | GREEN when |
| --- | --- |
| **1 Release Safety** | preflight runs in the Action with strict mode and exit codes; release notes are checked; `make publish` gates on the generic preflight; CI green |
| **2 Artifact & Formula** | noarch archives generate a valid formula installed by real Homebrew on macOS and Linux; duplicate installs fail generation; goldens pass `brew style` |
| **3 Recovery** | `hephaestus status` classifies every state read-only; each of the 12 scenarios in [recovery.md](../recovery.md) is covered by a test or a documented manual procedure |
| **4 CI & Security** | tests on Linux amd64/arm64, macOS arm64/amd64 (or documented as unverified), Windows amd64; actions pinned by SHA; no token in logs, summaries, reports; least-privilege permissions |
| **5 Compatibility** | policy and stable surface published; v0.2.0 configurations load unchanged in CI |
| **6 Developer Experience** | a new project can start from a template for Go, Rust, a script (noarch) and prebuilt binaries; docs set complete in English, README in English and Japanese consistent |
