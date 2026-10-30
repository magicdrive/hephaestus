# Hephaestus v1.5.0

Hephaestus can now be installed without Homebrew or Go: `install.sh` installs a release from
GitHub Releases on macOS and Linux (amd64, arm64), checked against the release's checksum file.
Nothing else changes: the commands, the configuration, the release pipeline and the action are
those of v1.4.0.

## Install script

```sh
curl -fsSLO https://raw.githubusercontent.com/magicdrive/hephaestus/main/install.sh
sh install.sh                        # the latest stable release, into ~/.local/bin
sh install.sh --version v1.4.0       # a given release (older ones and pre-releases too)
sh install.sh --upgrade              # the latest stable release again
sh install.sh --dir "$HOME/bin"      # another directory (created if missing)
```

- The archive must match the SHA-256 of `hephaestus_checksums.txt`; a missing or malformed
  checksum file stops the install. Only the `hephaestus` binary is taken out of the archive,
  after its entries have been checked (no absolute paths, `..`, links or devices), and it
  replaces the installed one with a single rename after it ran and reported the version asked
  for. A failed or interrupted run leaves the installed binary as it was.
- It never uses `sudo` and never changes `PATH`; it refuses Homebrew's directories and does not
  replace a symbolic link (keep `brew upgrade hephaestus` for a Homebrew installation).
- Every release from v0.1.0 on can be installed. The archives keep carrying build provenance
  attestations (`gh attestation verify <file> --repo magicdrive/hephaestus`).
- Windows is not supported by the script (use the `.zip` of the release, or the action).

The script is the installer of [Enma](https://github.com/magicdrive/enma) with one more
configuration line, `VERSION_PREFIX`, because `hephaestus --version` prints `hephaestus v1.5.0`
(unchanged). See [docs/installer.md](https://github.com/magicdrive/hephaestus/blob/v1.5.0/docs/installer.md).

## Documentation

- README: the Installation section shows Homebrew, the install script, the archives, the action
  and building from source. `go install github.com/magicdrive/hephaestus@latest` is no longer
  documented.

## Verification

- `internal/installer` runs the script against a fake github.com (local TLS, no network) with
  sh, dash, bash, ksh and BusyBox, curl and GNU Wget: install, reinstall, upgrade, downgrade,
  pre-releases, HTTP failures, truncated and stalled downloads, checksum and archive attacks,
  install directories, Homebrew, interrupted and concurrent runs, and the README's commands.
- CI installs the snapshot archives it has just built with the script (`TestDistArchives`).

**Full Changelog**: https://github.com/magicdrive/hephaestus/compare/v1.4.0...v1.5.0
