# v1.0.0 progress

Working record of the road to v1.0.0 ([v1-roadmap.md](v1-roadmap.md)).
The stack was developed without merging; for v1.0.0-rc.1 the owner
authorized merging it in order and publishing the release candidate (not
v1.0.0). v1.0.0-rc.1 was published from #14's merge (fe71b01) and
v1.0.0-rc.2 from #17's merge (1b462d3); both are immutable prereleases.
v1.0.0 is prepared by #19 and published only after the owner merges it.

## Pull requests (stacked)

| # | Branch | Base | Phase | Content |
| --- | --- | --- | --- | --- |
| [#5](https://github.com/magicdrive/hephaestus/pull/5) | `feat/release-safety-integration` | `main` | 1 | preflight in the Action, release-notes check, `make publish` gate |
| [#6](https://github.com/magicdrive/hephaestus/pull/6) | `feat/noarch-formula-quality` | #5 | 2 | noarch archives, duplicate-install detection, noarch Homebrew E2E |
| [#7](https://github.com/magicdrive/hephaestus/pull/7) | `feat/release-recovery` | #6 | 3 | `hephaestus status`, recovery tests, recovery guide |
| [#8](https://github.com/magicdrive/hephaestus/pull/8) | `docs/v1-stable-contract` | #7 | 0, 6 | roadmap, compatibility policy, v0.2.0 regression fixtures |
| [#9](https://github.com/magicdrive/hephaestus/pull/9) | `test/release-e2e-security` | #8 | 4 | full-pipeline E2E, Linux arm64 / macOS Intel CI, workflow hardening |
| [#10](https://github.com/magicdrive/hephaestus/pull/10) | `feat/developer-experience` | #9 | 5 | `hephaestus init` templates |
| [#11](https://github.com/magicdrive/hephaestus/pull/11) | `test/v1-stabilization` | #10 | 8 | CLI reference, changelog, this record |
| [#12](https://github.com/magicdrive/hephaestus/pull/12) | `fix/published-release-immutability` | #11 | RC audit | a published release is never edited; tap-failure and immutable E2E; prerelease tap guard in `release.yml` |
| [#13](https://github.com/magicdrive/hephaestus/pull/13) | `docs/v1.0.0-rc.1-notes` | #12 | RC | release notes and changelog for v1.0.0-rc.1 |
| [#14](https://github.com/magicdrive/hephaestus/pull/14) | `fix/ci-preflight-smoke-unknown` | `main` | RC | action smoke accepts an unknown preflight on push and tag checkouts |
| [#15](https://github.com/magicdrive/hephaestus/pull/15) | `fix/snapshot-skips-formula` | `main` | rc.1 dogfooding | snapshot skips formula; init formula test; lagging tap step data |
| [#16](https://github.com/magicdrive/hephaestus/pull/16) | `fix/never-delete-releases` | `main` | rc.2 audit | no release is ever deleted; re-reads before publishing, deleting assets and updating drafts; published assets never replaced |
| [#17](https://github.com/magicdrive/hephaestus/pull/17) | `docs/v1.0.0-rc.2-notes` | `main` | rc.2 | release notes and changelog for v1.0.0-rc.2 |
| [#18](https://github.com/magicdrive/hephaestus/pull/18) | `ci/verify-release-workflow` | `main` | rc.2 verification | `verify-release.yml`: run a published release's binaries on six platforms |
| #19 | `docs/v1.0.0-stable-release` | `main` | 1.0 | release notes, changelog and docs for v1.0.0 |

Merge in order; each base branch is merged into the next, so no rebase is
needed.

## Phases

| Phase | State |
| --- | --- |
| 0 Baseline audit | done ([v1-roadmap.md](v1-roadmap.md)) |
| 1 Release Safety | done (#5) |
| 2 Artifact & Formula | done (#6) |
| 3 Recovery | done (#7) |
| 4 CI & Security | done (#9) |
| 5 Developer Experience | done (#10) |
| 6 Compatibility contract | done (#8) |
| 7 Dogfooding | done (below) |
| 8 Stabilization | done (this pull request: docs and changelog only) |

## Dogfooding (read-only; no change to magicdrive/t or the production tap)

| Project | What was run | Result |
| --- | --- | --- |
| Hephaestus (Go) | `make snapshot`, `make publish-check` | 6 archives verified; the publish gate fails on stale release notes as intended |
| magicdrive/t (Bash) | `check`, `preflight --tag v1.2.1`, `status --tag v1.2.0` | PASS (23 pass, 3 skip); the local rebuild of v1.2.0 is identical to the published immutable assets; one changed line is a conflict |
| magicdrive/t as noarch (scratch copy) | archive, formula, then `brew style`, `install`, `test` in `homebrew/brew` | one archive, one url; installed with the bash completion link; `t --help` runs |
| Rust fixture (`cargo new`, `init --template rust`) | build, archive, checksum, formula on macOS x86_64 | the archived binary runs; other targets need their Rust targets installed |
| Published v1.0.0-rc.2 binary | `init` (go, rust, script, prebuilt) → `check` → `run --snapshot` → formula → `brew style`, `audit --strict`, `install`, `test`, `uninstall`; magicdrive/t read-only (`preflight`, `status`, formula); rebuild of the tag; `verify-release` on six platforms | all passed; the tag rebuild is identical to the published assets; t's formula is byte-identical to the tap's; Rust on the host target only |

## Platforms

| Platform | Evidence |
| --- | --- |
| Linux amd64 | CI `test`, `race`, `homebrew-e2e`, `action-smoke`, `action-boundary` |
| Linux arm64 | CI `test (ubuntu-24.04-arm)` on every pull request |
| macOS arm64 | CI `test`, `race`, `homebrew-e2e`, action jobs (`macos-latest`) |
| macOS amd64 | `ci-extended` (`macos-15-intel`); local development machine (x86_64) |
| Windows amd64 | CI `test`, `action-smoke`, `action-boundary` |
| Windows arm64 | published v1.0.0-rc.2 binary run by `verify-release` (`windows-11-arm`); no tests from source |

## Open items

- Machine-readable configuration schema (optional; not planned for 1.0).
- Rust template cross-targets (aarch64-apple-darwin, *-linux-musl) are not
  built in CI or locally; only the host target was verified.
