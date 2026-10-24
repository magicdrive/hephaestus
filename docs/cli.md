# CLI reference

```text
hephaestus <command> [flags]
```

Every command reads `hephaestus.yaml` from the current directory unless
`--config` names another file. `hephaestus <command> -h` prints the flags.

| Command | Writes | Purpose |
| --- | --- | --- |
| `init` | one new file | starting configuration from a template |
| `check` | nothing | validate the configuration, print the resolved plan |
| `run` | `dist/`, GitHub (release, tap) | run the configured stages |
| `build`, `archive`, `checksum`, `release`, `formula`, `tap` | as `run` | run one stage (same flags as `run`) |
| `preflight` | nothing | read-only release checks before pushing a tag |
| `status` | nothing | read-only state of a tag's release and the next safe action |
| `version` | nothing | print the version |

## init

```sh
hephaestus init --template go|rust|script|prebuilt [--repo owner/name]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--template` | required | `go` (cross-compiled with `go build`), `rust` (`cargo build --target`, one Rust target per platform), `script` (one [noarch](archives.md#noarch-archives) archive), `prebuilt` (executables at `prebuilt/<os>_<arch>/<name>`) |
| `--name` | the directory name | command, archive and formula name |
| `--repo` | `$GITHUB_REPOSITORY` | release repository; required when unknown |
| `--tap` | `<owner>/homebrew-tap` | tap repository |
| `--license` | `MIT` | SPDX license |
| `--description` | `Command-line tool` | one line; Homebrew wants it capitalized and not starting with the name |
| `--output` | `hephaestus.yaml` | file to write; an existing file is never overwritten |

Exit codes: `0` written, `1` the file exists or cannot be written, `2` usage.

## check, run and the stage commands

| Flag | Default | Meaning |
| --- | --- | --- |
| `--tag` | `$GITHUB_REF_NAME` on tag pushes | release tag |
| `--snapshot` | off | build without a tag (`0.0.0-SNAPSHOT`); release and tap stages are skipped, and refused when named |
| `--stages` (`run`) | every configured stage | comma-separated stages, or `all` |
| `--phase` (`run`) | — | `local` (build, archive, checksum) or `publish` (release, formula, tap) |
| `--fresh` | off | ignore an existing manifest instead of continuing from it |
| `--dry-run` | off | plan release and tap publication without writing to GitHub |
| `--commit` | `$GITHUB_SHA` | commit SHA the release is made from |
| `--repo` | config, then `$GITHUB_REPOSITORY` | release repository |
| `--token-env` | `GITHUB_TOKEN` | variable holding the release token |
| `--tap-token-env` | `HOMEBREW_TAP_GITHUB_TOKEN` | variable holding the tap token |

Exit codes: `0` success, `1` an invalid configuration or a failed stage,
`2` invalid flags. Re-running after a failure resumes from the manifest
([recovery.md](recovery.md)).

## preflight

```sh
hephaestus preflight --tag v1.2.3 [--strict] [--format json] [--report FILE]
```

Read-only checks before pushing a tag; the checks, flags and statuses are in
[preflight.md](preflight.md). Exit codes: `0` pass, `1` a failure (with
`--strict`, also a warning or unknown), `2` usage, `3` some checks unknown.

## status

```sh
hephaestus status --tag v1.2.3 [--format json]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--tag` | required | the release tag |
| `--repo` | config, then `$GITHUB_REPOSITORY` | release repository |
| `--format` | `text` | `text` or `json` |
| `--token-env` | `GITHUB_TOKEN` (then `GH_TOKEN`) | token for reading |

States and their safe recovery are in [recovery.md](recovery.md). Exit
codes: `0` read, `1` conflict, `2` usage, `3` GitHub could not be read.
