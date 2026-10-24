# Releasing Hephaestus

Hephaestus releases itself with Hephaestus, from `hephaestus.yaml` at the
repository root. Releases are cut only by pushing a tag; the Makefile never
tags, pushes or publishes.

## One-time setup

| Item | Value |
| --- | --- |
| Environment | `release` (Settings → Environments), with deployment restricted to tags `v*` and, optionally, required reviewers |
| Secret in that environment | `HOMEBREW_TAP_GITHUB_TOKEN`: a GitHub App installation token or fine-grained personal access token with **contents: write** and **pull-requests: write** on `magicdrive/homebrew-tap` only |
| Release token | The workflow's `GITHUB_TOKEN` (`contents: write` is granted to the release job only) |
| Repository settings | Immutable releases may be enabled: Hephaestus publishes only complete, verified drafts |

## Cutting a release

1. Make sure `main` is green and the changes are described in the merged
   pull requests.
2. Tag and push:

   ```sh
   git tag -a v1.2.3 -m "v1.2.3"
   git push origin v1.2.3
   ```

   A tag with a pre-release part (`v1.3.0-rc.1`) is published as a
   prerelease automatically.
3. `.github/workflows/release.yml` runs:
   1. **ci** — the complete CI (`ci.yml`) at the tagged commit; nothing is
      published unless `ci-gate` passes.
   2. **build** (`contents: read` only — no token that can write and no
      OIDC token):
      - `make build` — build Hephaestus from the tagged source;
      - `hephaestus run --stages build,archive,checksum --fresh` — all six
        targets, archives and `hephaestus_checksums.txt`;
      - `scripts/verify-dist.sh dist` — every archive's contents and modes,
        the checksums, and the host binary runs;
      - upload `dist/` as the workflow artifact `dist`.
   3. **release** (environment `release`; runs no project build command):
      - `make build` — build Hephaestus itself to publish with;
      - download `dist/` and run `scripts/verify-dist.sh dist` again;
      - for a stable tag, `hephaestus run --stages formula,tap --dry-run`
        with the tap token — read-only checks of the token, the tap's
        formula directory and the pull request branch, before the release
        becomes public;
      - `actions/attest-build-provenance` — provenance for the archives and
        the checksum file;
      - `hephaestus run --stages release` with `GITHUB_TOKEN` — draft,
        upload, verify, publish, with GitHub-generated notes
        (`generate_notes: true`);
      - `hephaestus run --stages formula,tap` with
        `HOMEBREW_TAP_GITHUB_TOKEN` — the formula and a pull request on
        `magicdrive/homebrew-tap` (skipped for prereleases:
        `skip_upload: auto`).
4. Review and merge the tap pull request.

A failed run can be re-run: the release is still a draft (or complete), and
every step skips what is already in place.

## Rehearsing a release

`.github/workflows/release-rehearsal.yml` (run manually, from the default
branch) runs the release path against the real GitHub API without
publishing: the same CI gate and read-only build job, then a publish job in
the `release-rehearsal` environment with `hephaestus.rehearsal.yaml`. That
file equals `hephaestus.yaml` (enforced by `repo_test.go`) except that the
release stays a **draft** (GitHub creates no tag for a draft, and nothing
is public) and the formula pull request goes to
`magicdrive/homebrew-tap-sandbox`. Each publishing stage runs twice to show
that a re-run is a no-op. Provenance attestation is not rehearsed, because
it writes a public transparency log entry.

| Item | Value |
| --- | --- |
| Environment | `release-rehearsal`, deployment restricted to the default branch |
| Secret in that environment | `HOMEBREW_TAP_GITHUB_TOKEN`: fine-grained token with contents and pull-requests write on `magicdrive/homebrew-tap-sandbox` only |
| Sandbox tap | `magicdrive/homebrew-tap-sandbox`, formulae at the root like the production tap |

```sh
gh workflow run release-rehearsal.yml -f version=0.1.0-rehearsal.1
```

The run summary prints the cleanup commands (delete the draft release,
close the sandbox pull request and delete its branch).

## Verifying a release

```sh
gh release download v1.2.3 --repo magicdrive/hephaestus
sha256sum -c hephaestus_checksums.txt        # shasum -a 256 -c on macOS
gh attestation verify hephaestus_1.2.3_linux_amd64.tar.gz --repo magicdrive/hephaestus
```

`gh attestation verify` checks that the archive was built by this
repository's release workflow from the tagged commit. Archive packaging is
deterministic, so rebuilding the tag from a clean checkout with the same Go
toolchain (`make build && ./build/hephaestus run --tag v1.2.3 --stages
build,archive,checksum --fresh`) is expected to reproduce the published
checksums; any difference comes from the compiled binaries.

## Snapshot locally

```sh
make snapshot     # build, archive, checksum and verify into dist/
```
