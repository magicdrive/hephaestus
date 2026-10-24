# Compatibility policy

From v1.0.0, Hephaestus follows [Semantic Versioning](https://semver.org/).
This page defines what is covered — the **stable surface** — and how it may
change. Anything not listed here is internal and may change in any release.

## Stable surface

| Surface | Stable | Not covered |
| --- | --- | --- |
| **Configuration** (`hephaestus.yaml`, `version: 1`) | every documented key, its type, default and meaning ([configuration.md](../configuration.md), [archives.md](../archives.md)); a valid v1 file stays valid and produces the same artifacts | the wording of error messages; the order of warnings |
| **CLI** | command names, documented flags, exit codes of `preflight` (0/1/2/3) and `status` (0/1/2/3), `run`'s 0 success / 1 failure / 2 invalid flags | human-readable output text (use `--format json`) |
| **JSON reports** | `preflight --format json` / `--report` (`schema_version: 1`), `status --format json`: existing fields keep name, type and meaning; check IDs keep their meaning | new fields, new check IDs, new status values for `status` assets/taps (consumers must ignore unknown ones) |
| **Artifact Manifest** (`dist/hephaestus-manifest.json`, `schema_version`) | existing fields; a manifest written by 1.x is readable by later 1.x | fields added |
| **GitHub Action** (`action.yml`) | input and output names, their meaning, the `command` values | the internal steps; how the binary is obtained |
| **Artifacts** | archive names from the same templates, checksum file format (`<sha256>  <name>`), formula semantics (url, sha256, install, test) | byte-level formula layout beyond `brew style`; ordering of stanzas Homebrew does not care about |
| **Safety guarantees** | never move or delete a tag, never overwrite a differing asset, never write to an immutable release's assets, never force-push a tap branch, never merge a tap pull request, read-only `preflight` and `status` | — |

## What each version may change

| Release | May | May not |
| --- | --- | --- |
| **Patch** (1.0.x) | fix bugs, tighten a check that was wrong, improve messages | add configuration keys; change defaults |
| **Minor** (1.x.0) | add configuration keys (optional, with defaults that keep the old behavior), commands, flags, Action inputs/outputs, JSON fields, preflight checks; deprecate | remove or rename anything stable; change a default's behavior; make a passing v1 configuration fail |
| **Major** (2.0.0) | remove deprecated surface, change defaults, `version: 2` configuration | silently reinterpret a `version: 1` file |

A new preflight check may make a previously passing release **WARN**; it may
make it **FAIL** in a minor release only when the release it blocks would
not have worked (for example, release notes that name another version).

Security fixes are the exception: if keeping a behavior would expose tokens
or allow writing outside the project, it changes in a patch release, with a
note in the changelog.

## Deprecation

A deprecated key, flag or input keeps working for at least one minor release
and until the next major, and produces a warning naming its replacement.
Deprecations are listed in [CHANGELOG.md](../../CHANGELOG.md).

## Configuration schema evolution

- `version: 1` (or omitted) is the only schema. A file with another version
  is rejected, never guessed.
- Unknown keys are errors with a "did you mean" suggestion, so a typo never
  silently changes a release. New keys therefore require a newer Hephaestus;
  an older binary rejects a file that uses them instead of ignoring them.
- GoReleaser `brews` compatibility is recorded field by field in
  [goreleaser.md](goreleaser.md); a field's status changes only in a minor
  (to more support) or major release.

## How it is enforced

- `testdata/compat/v0.2.0/` holds configurations that worked with v0.2.0
  (Hephaestus's own and magicdrive/t's, verbatim);
  `internal/config/compat_test.go` loads them on every CI run.
- Formula goldens (`internal/homebrew/testdata/*.golden`) are checked by
  `brew style` and pin the generated formulas.
- `testdata/compat/goreleaser/` pins GoReleaser import behavior.
- The preflight JSON schema and check IDs are pinned by
  `internal/preflight` tests and the docs test that every check ID is
  documented.
