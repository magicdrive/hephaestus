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
   2. **release** (environment `release`):
      - `make build` — build Hephaestus from the tagged source;
      - `hephaestus run --stages build,archive,checksum --fresh` — all six
        targets, archives and `hephaestus_checksums.txt`;
      - `scripts/verify-dist.sh dist` — every archive's contents and modes,
        the checksums, and the host binary runs;
      - `actions/attest-build-provenance` — provenance for the archives and
        the checksum file;
      - `hephaestus run --stages release` with `GITHUB_TOKEN` — draft,
        upload, verify, publish, with GitHub-generated notes
        (`generate_notes: true`);
      - `hephaestus run --stages formula,tap` with
        `HOMEBREW_TAP_GITHUB_TOKEN` — the formula and a pull request on
        `magicdrive/homebrew-tap`.
4. Review and merge the tap pull request.

A failed run can be re-run: the release is still a draft (or complete), and
every step skips what is already in place.

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
