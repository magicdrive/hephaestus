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
| `publish` | the release tag (local and pushed) | release a version: preflight, confirm, push the tag, follow and verify the release workflow |
| `completion` | nothing | print the bash, zsh or fish completion script |
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
| `--snapshot` | off | build without a tag (`0.0.0-SNAPSHOT`); release, formula and tap stages (they need a tag) are skipped, and refused when named |
| `--stages` (`run`) | every configured stage | comma-separated stages, or `all` |
| `--phase` (`run`) | — | `local` (build, archive, checksum) or `publish` (release, formula, tap) |
| `--fresh` | off | ignore an existing manifest instead of continuing from it; archives, checksum files and formulae that manifest records in `dist` are deleted first when this run writes them again (only files unchanged since, never imported ones) |
| `--dry-run` | off | plan release and tap publication without writing to GitHub |
| `--commit` | `$GITHUB_SHA` | commit SHA the release is made from |
| `--repo` | config, then `$GITHUB_REPOSITORY` | release repository |
| `--token-env` | `GITHUB_TOKEN` | variable holding the release token |
| `--tap-token-env` | `HOMEBREW_TAP_GITHUB_TOKEN` | variable holding the tap token |
| `--workflows` (`check`) | off | also check the release workflow and the CI workflow it runs against the [workflow conventions](preflight.md#release-workflow) (the `workflow.*` checks of `preflight`), for the assets of `--tag` (default `v0.0.0`); a violation, or a repository that cannot be found, exits `1`. For CI on pull requests, before a release is tagged |

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

## publish

```sh
hephaestus publish [check|status|watch|plan|help] [flags] v1.2.3
```

Releases a version of the project in the current directory: a read-only
preflight, a confirmation, one annotated tag pushed without force, then it
follows the `release.yml` run the push starts and verifies the release
(assets, checksums, attestations, Homebrew tap). It never publishes or writes
to GitHub itself. Without a command it releases; `check` runs the preflight
only, `status` shows what exists for a version, `watch` follows an already
pushed tag, `plan` prints `tag=`, `version=`, `prerelease=` and `tap=` lines
for the release workflow, `help` describes the safety rules. Flags come before
the version.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config` | `hephaestus.yaml` | configuration file |
| `-yes` | off | confirm without the prompt |
| `-from-make` | off | read the version from `HEPHAESTUS_PUBLISH_ARGS`, `HEPHAESTUS_PUBLISH_VERSION` or `HEPHAESTUS_PUBLISH_TAG` (set by a Makefile) |
| `-discover-timeout` | `5m` | wait for the release run to appear |
| `-watch-timeout` | `90m` | wait for the release run to finish |
| `-poll` | `15s` | polling interval |

The project must follow publish's conventions (branch `main`, `ci.yml` with
`ci-gate`, `release.yml`, environment `release`, tag and main rulesets, the
tap step names). [publish.md](publish.md) lists them, with the Makefile for
`make publish vX.Y.Z` and the recovery steps. Exit codes: `0` released and
verified (`check`: passed; `status`: nothing failed), `1` failed or refused,
`2` usage or an invalid version, `3` outcome unknown (monitoring stopped; the
release may still be running).

## completion

```sh
hephaestus completion bash|zsh|fish
```

Prints the shell's completion script on stdout (nothing else; errors go to
stderr, an unknown shell exits `2`). The script completes commands, the
commands of `publish` (`check`, `status`, `watch`, `plan`, `help`), each
command's flags (`--flag` and `-flag`; a flag already given is not offered
again unless it is repeatable), and the fixed values of `--format`,
`--template`, `--phase`, `--stages` and `--skip` (comma-separated lists
continue after a comma), in both `--flag value` and `--flag=value` form.
`--config`, `--report` and `--output` complete file names. After `--`
nothing is offered. Tags, repositories and other free-form values are not
completed.

The script asks the running `hephaestus` for candidates (the hidden
`hephaestus __complete WORD...`), which only reads the command's own flag
definitions: no configuration, repository, token or network is used.

| Shell | Load for the current session | Install permanently (without Homebrew) |
| --- | --- | --- |
| bash (3.2 or later) | `eval "$(hephaestus completion bash)"` | `hephaestus completion bash > ~/.local/share/bash-completion/completions/hephaestus` (bash-completion 2), or the `eval` line in `~/.bashrc` |
| zsh | `source <(hephaestus completion zsh)` (after `compinit`) | `hephaestus completion zsh > ~/.zfunc/_hephaestus` with `fpath=(~/.zfunc $fpath)` before `compinit` in `~/.zshrc` |
| fish | `hephaestus completion fish \| source` | `hephaestus completion fish > ~/.config/fish/completions/hephaestus.fish` |

The Homebrew formula installs all three scripts; see the README's
[Shell completion](../README.md#shell-completion) section for enabling them.

