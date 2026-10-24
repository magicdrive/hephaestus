# Architecture overview

This page describes how a Hephaestus run is put together: the stages, the
Artifact Manifest that connects them, and which stage owns which data. The
invariants contributors must preserve are in
[ARCHITECTURE.md](../../ARCHITECTURE.md); repository conventions are in
[repository-conventions.md](repository-conventions.md).

## Stages

```text
                 hephaestus.yaml ──► config (strict load, defaults, validation)
                                          │
                                          ▼
                  pipeline.Check: every selected stage has its inputs?
                  (tag, repository, tokens, archives, formulas, ...)
                                          │ nothing runs if not
                                          ▼
 ┌────────┐   ┌─────────┐   ┌──────────┐   ┌─────────┐   ┌─────────┐   ┌─────┐
 │ build  │──►│ archive │──►│ checksum │──►│ release │──►│ formula │──►│ tap │
 └────────┘   └─────────┘   └──────────┘   └─────────┘   └─────────┘   └─────┘
  command      tar.gz/zip    sha256sum      GitHub        Homebrew      GitHub
  existing     from declared  file          Release       formula       contents/
  goreleaser   files                        (draft-first) (.rb)         refs/pulls API
     │             │             │              │             │            │
     └─────────────┴─────────────┴──────┬───────┴─────────────┘            │
                                        ▼                                  │
                     dist/hephaestus-manifest.json  ◄──────── reads ───────┘
```

- Stages always run in this order, whatever order `--stages` lists them in.
  `hephaestus run` without `--stages` runs the stages the configuration
  declares (`build` if `builds`, `archive` and `checksum` if `archives` and
  checksums are enabled, `release` if `release.github`, `formula` if `brews`,
  `tap` unless every formula has `skip_upload: true`).
- A stage can run alone (`hephaestus archive`, `hephaestus release`, ...) or
  in a later invocation, even in another job: it reads what earlier stages
  recorded in the manifest. Before anything runs, `pipeline.Check` verifies
  that every selected stage will have its inputs, counting what earlier
  stages of the same run will produce, and reports every missing input at
  once.
- The manifest is saved atomically after every stage, including a failed
  one, so a re-run continues from what was completed.

| Stage | Package | Reads | Writes on disk | Writes to GitHub |
| --- | --- | --- | --- | --- |
| build | `internal/build` | configuration; project files; GoReleaser `artifacts.json` | build outputs (command provider) | — |
| archive | `internal/archive` | manifest binaries (or explicit targets); project files | `<dist>/<name>.tar.gz` / `.zip` | — |
| checksum | `internal/checksum` | manifest archives; archive files | `<dist>/<checksum name>` | — |
| release | `internal/release` | manifest archives/checksums (and binaries if included); their files | — | release, assets |
| formula | `internal/homebrew` | manifest archives (checksums, URLs, contents) | `<dist>/homebrew/[directory/]<name>.rb` | — |
| tap | `internal/tap` | manifest formulae; formula files | — | branch, commit, pull request in the tap |

Shared packages: `internal/config` (schema, loading, validation),
`internal/tmpl` (templates), `internal/pathutil` and `internal/glob`
(path validation and matching), `internal/manifest`, `internal/pipeline`
(stage order and preflight), `internal/version` (tag parsing),
`internal/github` (REST client), `internal/ghaction` (step outputs and
summary), `internal/compat/goreleaser` (`brews` adapter). The CLI is
`cmd/hephaestus`; the GitHub Action (`action.yml`) builds and runs it.

## The Artifact Manifest

`dist/hephaestus-manifest.json` (configurable with `manifest`) is the
contract between stages. It is the only way stages pass data to each other.

```json
{
  "schema_version": 1,
  "project": "mycli",
  "version": "1.2.3",
  "tag": "v1.2.3",
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "artifacts": [
    {
      "kind": "binary",
      "id": "mycli",
      "name": "mycli",
      "path": "dist/bin/darwin_arm64/mycli",
      "os": "darwin",
      "arch": "arm64",
      "sha256": "…",
      "size": 4096000,
      "executables": ["mycli"],
      "source": { "stage": "build", "provider": "command" }
    }
  ]
}
```

Properties:

- **Versioned and strict.** `schema_version` must be `1`; unknown fields,
  trailing data, invalid kinds, paths that escape the project root and
  malformed digests are rejected. A manifest is at most 32 MiB.
- **Deterministic.** Artifacts are serialized sorted (kind, name, os, arch,
  arm) with two-space indentation; equal manifests are equal bytes.
- **Atomic.** Written to a temporary file and renamed.
- **Bound to one release.** A run that finds a manifest for another project
  or version stops and asks for `--fresh`; `--fresh` starts an empty one.
- **Unique names.** Two artifacts of the same kind may not share a name
  (binaries: per target), so release asset names cannot collide.

### Ownership

Each stage adds only artifacts of its own kind, or fills fields it owns. No
stage writes a value it did not observe: a checksum is present only when it
was computed from the bytes on disk or verified against them, and a download
URL only after the asset was uploaded and verified on a published release.

| Data | Written by | Fields | Replaced on re-run |
| --- | --- | --- | --- |
| Root (`schema_version`, `project`, `version`, `tag`, `commit`) | CLI, when the manifest is created | all | only with `--fresh` |
| `kind: binary` | build stage (`source.stage: build`, `provider`: `command`, `existing` or `goreleaser`; `origin`: the `artifacts.json` path for GoReleaser) | `id`, `name`, `path`, `os`, `arch`, `arm`, `sha256`, `size`, `executables`, `source` | binaries of the build ids that ran |
| `kind: archive` (built) | archive stage (`source.stage: archive`) | `id`, `name`, `path`, `os`, `arch`, `arm`, `format`, `sha256`, `size`, `executables`, `files`, `source` | archives of the archive ids that ran; imported archives are kept |
| `kind: archive` (imported) | build stage, `existing.kind: archive` or `goreleaser.archives: true` (`source.stage: build`) | same as built archives, `files` from inspection | archives imported by the build ids that ran |
| `kind: checksum` | checksum stage (`id: checksum`, `source.stage: checksum`) | `name`, `path`, `sha256`, `size` | always |
| `download_url` on uploaded artifacts | release stage | `download_url` | set again on every run that sees the published release |
| `kind: formula` | formula stage (`id`: formula name, `source.stage: homebrew`) | `name`, `path`, `sha256`, `size` | always |
| — | tap stage | reads formulae only | — |

Each stage validates the whole manifest after changing it.

## Data flow

1. **build** renders each target's command, output path and environment,
   runs the command (or locates the prebuilt file, or reads
   `artifacts.json`), verifies the file inside the project root, hashes it,
   and records `binary` (or imported `archive`) artifacts.
2. **archive** resolves each archive's `files` against the project root into
   an exact entry plan for every target, hashing each source; only when every
   plan succeeds are archives written, and each source is re-verified while
   writing. It records each archive with its content list.
3. **checksum** re-hashes every archive file, compares it with the manifest,
   and writes the checksums file.
4. **release** re-hashes every asset against the manifest, finds or creates a
   draft release, uploads and verifies each asset, publishes, then records
   download URLs.
5. **formula** selects darwin/linux amd64/arm64 archives, resolves URLs,
   generates the formula from manifest checksums and content lists, verifies
   the `install` paths against the archive contents, and records the formula.
6. **tap** checks the formula file against the manifest digest, then
   commits it to the tap through a pull request (default) or directly.

Snapshot runs (`--snapshot`) use version `0.0.0-SNAPSHOT` and no tag; the
release and tap stages refuse to run, and the formula stage requires a tag.
