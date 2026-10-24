# Recovering a release

A release that stops halfway can always be inspected without changing
anything, and resumed by running the same thing again. Hephaestus never
repairs a release by destroying what is already correct: it does not move or
delete tags, delete published releases, or overwrite assets that differ.

## Where does it stand?

```sh
hephaestus status --tag v1.2.3            # read-only; --format json for scripts
```

`status` reads the tag, the release (none, draft, published, immutable), its
assets compared by digest with the local Artifact Manifest
(`dist/hephaestus-manifest.json`, when it is of the same version) and each
formula's tap pull request, and prints the state and the next safe action:

| State | Meaning | Next |
| --- | --- | --- |
| `not-started` | no tag, no release | run the [preflight](preflight.md), push the tag |
| `tagged` | the tag exists, no release | look at the release workflow run; re-run its failed jobs |
| `draft` | a draft release exists | re-run the release: it resumes |
| `published-incomplete` | published, assets missing (mutable release) | re-run the release stage |
| `conflict` | an asset differs from the local build, or an immutable release is incomplete | stop: release a new version (see below) |
| `published` | assets complete; a tap pull request is missing, open or closed | re-run `--stages formula,tap`, or review the pull request |
| `completed` | published and every tap pull request merged | nothing |

Exit codes: `0` read, `1` conflict, `2` usage, `3` GitHub could not be read.

## Scenarios

| # | What failed | What is left | Safe recovery |
| --- | --- | --- | --- |
| 1 | **build** | the manifest without this stage's binaries; nothing on GitHub | fix the cause, run again (same tag; `--fresh` only to discard earlier stages) |
| 2 | **archive** | the manifest without new archives (a failed stage records nothing) | fix, run `--stages archive,checksum,...` again |
| 3 | **checksum** | the checksum file is rewritten from the manifest's archives each run | run again; a file changed after archiving is detected (digest) |
| 4 | **GitHub temporarily unavailable** (5xx, 429, network) | GET, PUT, PATCH and DELETE are retried with backoff (and `Retry-After`); POST never blindly: the release and asset creations re-read state first | run again if it still failed; nothing is duplicated |
| 5 | **after the draft was created** | a draft release with some assets | run again: the draft is found and reused, identical assets are skipped |
| 6 | **during an upload** | possibly a "starter" (incomplete) asset | run again: a completed upload whose response was lost is verified and kept; an old incomplete one is deleted and uploaded again (a recent one is left alone in case another run is uploading) |
| 7 | **asset digest mismatch** | an asset that differs from the local file | the upload is rejected (deleted, not kept); an **existing** asset that differs is an error, never overwritten unless `replace_existing_assets` on a mutable release. `status` shows `conflict` |
| 8 | **attestation** (workflow step before publishing) | nothing published | re-run the job; a repeated attestation of the same digests is harmless (each run adds one) |
| 9 | **tap pull request after publishing** | the published release | re-run only `hephaestus run --stages formula,tap` (or the failed job): the formula comes from the manifest, an existing branch or pull request with the same content is reused, a branch with other content is refused |
| 10 | **same tag again** | whatever earlier runs left | identical assets are no-ops; a tag pointing to another commit than the one built is an error before anything is written; a non-reproducible rebuild differs from the draft's assets and stops (`conflict`) |
| 11 | **two runs at once** | one draft (the lowest id wins) | the release workflow serializes runs per tag (`concurrency`); if two runs still race, the one that created a second draft deletes only its own draft and continues on the winner's |
| 12 | **re-run on an immutable release** | the published release | identical assets: no-op; anything else (missing or different assets) is an error before any write. Release notes, which GitHub keeps editable, may be updated |

## When the answer is "release a new version"

A published release is final. When `status` says `conflict` on a published
release, or the immutable release is incomplete, do not try to patch it:
fix the cause on the default branch and release the next version (for a
candidate, the next `-rc.N`). The tag stays as the record of what was
attempted; tag rules should forbid moving or deleting it.

A **draft** whose assets differ from a new build (the build is not
reproducible, or the source changed) is not overwritten either. Decide
deliberately: make the build reproducible and rebuild, or delete the draft
in the GitHub UI and run again.

## What is never done automatically

- moving, deleting or re-creating a tag;
- deleting a published release or replacing its assets (only with
  `replace_existing_assets`, only on a mutable release, never on an
  immutable one);
- deleting a draft this run did not create;
- force-pushing a tap branch, or merging a tap pull request;
- rolling back a partly published release.
