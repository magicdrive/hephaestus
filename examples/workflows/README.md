# GitHub Actions workflows

Copy one of these into `.github/workflows/` and replace
`magicdrive/hephaestus@<full-commit-sha>` with the commit you have reviewed.
The other actions are pinned to the commits this repository uses.

| File | Pattern |
| --- | --- |
| [release-single-job.yml](release-single-job.yml) | One job on a tag push: build/archive/checksum without tokens, then release, then formula and tap with the tap token. |
| [release-split-jobs.yml](release-split-jobs.yml) | Build in a read-only job, hand `dist/` to a publishing job as an artifact. |
| [goreleaser-formula-only.yml](goreleaser-formula-only.yml) | GoReleaser releases; Hephaestus imports its archives and publishes the formula. Pairs with [../goreleaser-formula-only](../goreleaser-formula-only/README.md). |
| [matrix-then-publish.yml](matrix-then-publish.yml) | Native builds on a runner matrix, then one publishing job with the `existing` provider. Pairs with [../existing-artifacts](../existing-artifacts/README.md). |
| [pull-request.yml](pull-request.yml) | Configuration check and snapshot build on pull requests, with no token. |

Every release workflow uses a `release` environment for the tap token and
`concurrency: release-${{ github.ref }}`. Token design and the rules for
untrusted pull requests are in [docs/github-action.md](../../docs/github-action.md).
