# Hephaestus v1.1.0

Shell completion, for Hephaestus itself and for the formulae it generates.
A minor release: it adds a command and a configuration key, as the
[compatibility policy](https://github.com/magicdrive/hephaestus/blob/v1.1.0/docs/compatibility/policy.md)
allows in 1.x.0.

## Hephaestus completes itself

`hephaestus completion bash|zsh|fish` prints a completion script for bash
(3.2 or later), zsh and fish. It completes:

- commands;
- each command's flags, in `--flag` or `-flag` form. A flag already given
  is not offered again, unless it can be repeated.
- the fixed values of `--format`, `--template`, `--phase`, `--stages` and
  `--skip`, as `--flag value` or `--flag=value`. Comma-separated lists
  continue after a comma.
- file names for `--config`, `--report` and `--output`.

Candidates come from the same flag definitions the commands parse, so
completion offers only what Hephaestus accepts. Generating them reads no
configuration, token or repository and uses no network.

**Homebrew users get the completions with this release.** The formula
installs the three scripts into Homebrew's completion directories, on a new
install and on `brew upgrade` from 1.0.0:

- `etc/bash_completion.d/hephaestus`
- `share/zsh/site-functions/_hephaestus`
- `share/fish/vendor_completions.d/hephaestus.fish`

Homebrew installs the files, but each shell must also be set up to load
them. A fish installed by Homebrew needs nothing more. bash needs
`bash-completion@2`, and zsh needs Homebrew's `site-functions` on `FPATH`
before `compinit`. The
[README](https://github.com/magicdrive/hephaestus/blob/v1.1.0/README.md#shell-completion)
has the lines to add, and the commands for installing without Homebrew.

## Completions in the formulae Hephaestus generates

`brews[].completions` installs bash, zsh and fish completions with a
formula, whatever language the CLI is written in. There are two ways to
provide them, and they can be combined for different shells:

- **Generated**: the installed executable prints each script at install
  time, through Homebrew's `generate_completions_from_executable`. You set
  the arguments (`args`, e.g. `[completion]`), how the shell is passed
  (`shell_parameter_format`: `arg`, `flag`, `none`, `cobra`, `clap`,
  `click` or `typer`) and the shells.
- **Shipped**: the scripts are included in the archive (`files`) and
  installed with `bash_completion.install`, `zsh_completion.install` and
  `fish_completion.install` under the names Homebrew expects.

```yaml
brews:
  - name: mycli
    completions:
      generate:
        args: [completion]
```

The configuration is checked before anything is published:

- the shell names and the format are valid;
- the executable is installed into `bin` for every archive;
- no shell's completion is installed twice;
- every shipped file exists in each archive.

Every value is written as a Ruby string literal. A formula without
`completions` is generated exactly as before. Details are in the
[configuration reference](https://github.com/magicdrive/hephaestus/blob/v1.1.0/docs/configuration.md#formula-completions).

## Testing

- **Real shells**: bash 5 and macOS's bash 3.2, zsh (through its completion
  system, in a pseudo-terminal) and fish, on Linux and macOS in CI. The
  cases cover commands, flags, values, `--flag=value`, comma lists, file
  names, a quoted value with a space, an already given flag, `--` and an
  invalid shell.
- **Homebrew**, with a shell-script fixture whose bash and zsh completions
  are generated and whose fish completion is shipped: `brew style`,
  `brew audit --strict`, install, the three files in place, completion in
  the three shells, `brew test`, uninstall.
- **Homebrew, upgrading Hephaestus itself**: v1.0.0 is installed from the
  production tap's formula, then upgraded with `brew upgrade` to a formula
  generated from this release's `hephaestus.yaml`. The three completion
  files appear and match `hephaestus completion`, completion works in the
  three shells with the installed binary, and `brew test` and uninstall
  pass.

## Compatibility

- Additions only: the `completion` command and the `brews[].completions`
  key. No existing command, flag, configuration key, Action input or
  output, JSON field or check ID changed. `hephaestus help` lists two more
  commands (`completion`, `help`).
- A configuration that uses `brews[].completions` is rejected by
  Hephaestus 1.0.0 and older, because unknown keys are errors.

## Limitations

- Tags, repositories and other free-form values are not completed.
- PowerShell completion is not provided. `generate` installs only bash,
  zsh and fish, including with `shell_parameter_format: cobra` or `typer`,
  whose Homebrew default would add PowerShell.
- A custom `shell_parameter_format` prefix string is not supported; use
  `extra_install` for such a case.

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.0.0...v1.1.0
