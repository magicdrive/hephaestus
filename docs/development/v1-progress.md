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
| — | `docs/v1-stable-contract` | #7 | 0, 6 | roadmap, compatibility policy, v0.2.0 regression fixtures |

Merge in order; each base branch is merged into the next, so no rebase is
needed.

## Phases

| Phase | State |
| --- | --- |
| 0 Baseline audit | done ([v1-roadmap.md](v1-roadmap.md)) |
| 1 Release Safety | done (#5) |
| 2 Artifact & Formula | done (#6) |
| 3 Recovery | done (#7) |
| 4 CI & Security | in progress |
| 5 Developer Experience | not started |
| 6 Compatibility contract | done (policy, fixtures) |
| 7 Dogfooding | partly: `status` against magicdrive/t v1.2.0 (identical rebuild, conflict on change) |
| 8 Stabilization | not started |

## Open items

- Machine-readable configuration schema (optional; not planned for 1.0).
- macOS Intel runner coverage.
