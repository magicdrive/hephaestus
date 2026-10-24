# v1.0.0 progress

Working record of the road to v1.0.0 ([v1-roadmap.md](v1-roadmap.md)).
No merges, tags or releases are made by the development agent; each pull
request is reviewed and merged by the owner.

## Pull requests (stacked)

| # | Branch | Base | Phase | Content |
| --- | --- | --- | --- | --- |
| [#5](https://github.com/magicdrive/hephaestus/pull/5) | `feat/release-safety-integration` | `main` | 1 | preflight in the Action, release-notes check, `make publish` gate |
| [#6](https://github.com/magicdrive/hephaestus/pull/6) | `feat/noarch-formula-quality` | #5 | 2 | noarch archives, duplicate-install detection, noarch Homebrew E2E |
| [#7](https://github.com/magicdrive/hephaestus/pull/7) | `feat/release-recovery` | #6 | 3 | `hephaestus status`, recovery tests, recovery guide |
| [#8](https://github.com/magicdrive/hephaestus/pull/8) | `docs/v1-stable-contract` | #7 | 0, 6 | roadmap, compatibility policy, v0.2.0 regression fixtures |
| [#9](https://github.com/magicdrive/hephaestus/pull/9) | `test/release-e2e-security` | #8 | 4 | full-pipeline E2E, Linux arm64 / macOS Intel CI, workflow hardening |
| [#10](https://github.com/magicdrive/hephaestus/pull/10) | `feat/developer-experience` | #9 | 5 | `hephaestus init` templates |
| — | `test/v1-stabilization` | #10 | 8 | CLI reference, changelog, this record |

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

## Platforms

| Platform | Evidence |
| --- | --- |
| Linux amd64 | CI `test`, `race`, `homebrew-e2e`, `action-smoke`, `action-boundary` |
| Linux arm64 | CI `test (ubuntu-24.04-arm)` on every pull request |
| macOS arm64 | CI `test`, `race`, `homebrew-e2e`, action jobs (`macos-latest`) |
| macOS amd64 | `ci-extended` (`macos-15-intel`); local development machine (x86_64) |
| Windows amd64 | CI `test`, `action-smoke`, `action-boundary` |
| Windows arm64 | unverified (archives are built; no runner) |

## Open items

- Machine-readable configuration schema (optional; not planned for 1.0).
- `config.release-notes`: RELEASE_NOTES.md and CHANGELOG.md must name the
  v1.0.0 tag before it is pushed (owner's release step).
