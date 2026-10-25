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
| 6 | **during an upload** | possibly a "starter" (incomplete) asset | run again: a completed upload whose response was lost is verified and kept; on a draft, an incomplete one older than 10 minutes is deleted and uploaded again; a recent one is left alone, even right after this run's own failed attempt, since it cannot be told apart from another run's upload in progress. On a published release an incomplete asset is reported, never deleted |
| 7 | **asset digest mismatch** | an asset that differs from the local file | the upload is rejected (deleted, not kept); an **existing** asset that differs is an error, never overwritten (`replace_existing_assets` replaces it on a draft only, never on a published release). `status` shows `conflict` |
| 8 | **attestation** (workflow step before publishing) | nothing published | re-run the job; a repeated attestation of the same digests is harmless (each run adds one) |
| 9 | **tap pull request after publishing** | the published release | re-run only `hephaestus run --stages formula,tap` (or the failed job): the formula comes from the manifest, an existing branch or pull request with the same content is reused, a branch with other content is refused |
| 10 | **same tag again** | whatever earlier runs left | identical assets are no-ops; a tag pointing to another commit than the one built is an error before anything is written; a non-reproducible rebuild differs from the draft's assets and stops (`conflict`) |
| 11 | **two runs at once** | the winning release (published, else the lowest-id draft) and possibly an empty duplicate draft | the release workflow serializes runs per tag (`concurrency`). If two runs still race, the one whose new draft loses stops with a **conflict** and deletes nothing; its empty draft is left for you to delete. Right before publishing, every run re-reads the draft: one published meanwhile is not edited, and a draft whose assets changed or that lost to another release is not published. Re-run once the other run has finished |
| 12 | **re-run on an immutable release** | the published release | identical assets: no-op; anything else (missing or different assets) is an error before any write. Title, notes and prerelease flag are never edited (on any published release); a difference is a warning |

## What each state allows

The tag is never moved or deleted in any state; a tag that points to
another commit than the one built is an error before anything is written.

| Release | May be written | Never written |
| --- | --- | --- |
| none | a draft is created (after a lookup, never a blind second create), assets uploaded, then published | — |
| draft | missing assets uploaded; incomplete leftovers (older than the grace period) deleted; title, notes and prerelease flag updated; published once complete and re-read | a differing asset (unless `replace_existing_assets`); publishing with assets not in the manifest, or after its assets changed; deleting the draft |
| published, mutable | missing assets uploaded ("published-incomplete") | any existing asset (replacing or deleting it, even with `replace_existing_assets`, or an incomplete one); title, notes, prerelease flag; unpublishing; deleting the release |
| published, immutable | nothing: assets are compared read-only | any asset, title, notes or prerelease flag; deleting the release |
| conflict | nothing | the differing asset |

A difference in title, notes or prerelease flag on a published release is
logged as a warning; the run continues, so formula and tap can resume.

Hephaestus never deletes a release, in any state: its GitHub client has no
call for it. The only asset it may delete from a published release is one
it has just uploaded itself and whose verification failed (the id comes
from its own upload response).

A draft is also re-read right before one of its assets is deleted (for
`replace_existing_assets` or an old incomplete upload) and before its
title or notes are updated with `draft: true`: a release published
meanwhile is left alone. After publishing, each asset is checked against
this build's digest before its URL is recorded in the manifest.

Known limit: GitHub has no conditional update, so between a re-read and
the write that follows it (one API call) another writer could still change
the release.

## How each scenario is verified

| # | Verified by |
| --- | --- |
| 1, 2 | CLI integration test: `TestResumeAfterStageFailure` (a stage fails, the re-run resumes from the manifest); build failures: `internal/build` tests |
| 3 | unit tests: `TestTamperDetected` (checksum), `TestSourceChangedBetweenPlanAndWrite` (archive) |
| 4 | unit tests: `internal/github` retry tests; `TestCreateFailureDoesNotDuplicateRelease`, `TestRateLimitRetries` |
| 5 | unit test `TestReusesExistingDraft`; E2E `recovery` |
| 6 | unit tests: `TestRecoverFromUpload502LeavingStarter`, `TestRecoverDeletesOldStarterAfterFailedUpload`, `TestUploadLostResponseIsVerifiedNotRepeated`, `TestRecentStarterAssetIsNotDeleted`, `TestPublishedReleaseAssetsAreNeverReplacedOrDeleted` |
| 7 | unit tests: `TestExistingAssetSameDifferentReplace`, `TestDigestConflictOnPublishedRelease`, `TestImmutablePublishedRelease`, `TestPublishedReleaseAssetsAreNeverReplacedOrDeleted`; `status` conflict: `internal/status` tests |
| 8 | **documentation only**: the attestation is a workflow step (`release.yml`), exercised by every real release, not by a test |
| 9 | E2E `tap-failure` (pull request creation fails after publishing; the full re-run writes nothing to the release and opens the pull request) and `tap-only`; tap unit tests for reused branches and pull requests |
| 10 | unit tests: `TestRerunIsNoOp`, `TestExistingTagMustPointToBuiltCommit`; E2E `stable` (second run writes nothing) |
| 11 | unit tests: `TestConcurrentDraftLoserStopsWithoutDeleting` (lower-id draft, published release), `TestConcurrentDraftWinnerKeepsItsDraft`, `TestLostCreateDoesNotClaimAnotherRunsDraft`, `TestCheckBeforePublish` (published, asset added, changed or removed, another release published meanwhile), `TestDraftPublishedBeforeAssetDeleteKeepsAsset`, `TestDraftConfigDoesNotEditReleasePublishedMeanwhile`, `TestPublishedMeanwhileWithOtherContentIsNotRecorded`, `TestClientCannotDeleteReleases`; the workflow's `concurrency` group is configuration, not tested |
| 12 | unit tests: `TestImmutablePublishedRelease`, `TestPublishedReleaseIsNeverEdited`, `TestImmutableReleaseRerunWritesNothing`; E2E `immutable` and `immutable-resumes-tap` |
| lost publish response | unit test `TestPublishLostResponseIsNotPublishedTwice` |

Unit and E2E tests run against a fake GitHub API (`internal/release`,
`cmd/hephaestus/pipeline_e2e_test.go`). Against the real GitHub API, only
the normal path is exercised (every published release); failure injection
is not.

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
- deleting a release (published or draft, including a duplicate draft this
  run created);
- replacing or deleting an asset of a published release (mutable or
  immutable; `replace_existing_assets` applies to drafts only);
- deleting an incomplete asset that may be another run's upload in progress
  (updated within the last 10 minutes);
- force-pushing a tap branch, or merging a tap pull request;
- rolling back a partly published release.
