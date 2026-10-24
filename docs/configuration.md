# Configuration reference

Hephaestus is configured by one YAML file, `hephaestus.yaml`. This page lists
every field, its default and its constraints, as implemented in
`internal/config` (`types.go`, `brew.go`, `validate.go`) and `internal/tmpl`.
Archive file lists have their own page: [archives.md](archives.md).

```yaml
version: 1

project:
  name: mycli
  description: Friendly tool for managing widgets
  homepage: https://github.com/acme/mycli
  license: MIT

builds:
  - id: mycli
    matrix:
      os: [darwin, linux, windows]
      arch: [amd64, arm64]
    command: [go, build, -trimpath, -o, "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}", .]
    env:
      GOOS: "{{ .OS }}"
      GOARCH: "{{ .Arch }}"
      CGO_ENABLED: "0"
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"

archives:
  - id: default
    format_overrides:
      - os: windows
        format: zip
    files:
      - src: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"
        dst: "mycli{{ .Ext }}"
        mode: "0755"
      - LICENSE
      - README.md

checksum:
  name: "{{ .Project }}_checksums.txt"

release:
  github:
    owner: acme
    name: mycli

brews:
  - name: mycli
    repository:
      owner: acme
      name: homebrew-tap
```

## Loading rules

- **File lookup.** Without `--config`, the first of `hephaestus.yaml`,
  `hephaestus.yml`, `.hephaestus.yaml`, `.hephaestus.yml` in the current
  directory is used. The directory that contains the file is the **project
  root**: every relative path in the configuration is resolved against it.
- **Strict parsing.** Unknown keys are errors, reported with their line and a
  "did you mean" suggestion. Duplicate keys are errors. YAML anchors and
  aliases are rejected. The file must be a single YAML document of at most
  1 MiB.
- **All problems at once.** Validation collects every error, each with its
  YAML line and field path (`line 12: archives[0].files[2].dst: ...`).
- **Warnings.** Deprecated GoReleaser names (`brews[].tap`, `brews[].folder`)
  and accepted-but-ignored fields are printed as `warning:` lines; they never
  fail the run.
- `hephaestus check` loads and validates the file, prints the resolved stages,
  builds, archives and formulae, and runs nothing.

## Top-level fields

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `version` | integer | `1` | Schema version. Only `1` is supported. |
| `project` | mapping | required | See [project](#project). |
| `dist` | relative path | `dist` | Output directory for archives, the checksum file, formulae and the manifest. Not a template. |
| `manifest` | relative path | `<dist>/hephaestus-manifest.json` | Location of the [Artifact Manifest](architecture/overview.md#the-artifact-manifest). Not a template. |
| `env` | list of `KEY=VALUE` | none | Extra template variables (`{{ .Env.KEY }}`) and extra environment variables for build commands. Keys must match `^[A-Za-z_][A-Za-z0-9_]*$`. A key here overrides the process environment. |
| `builds` | list | none | See [builds](#builds). |
| `archives` | list | none | See [archives](#archives) and [archives.md](archives.md). |
| `checksum` | mapping | enabled | See [checksum](#checksum). |
| `release` | mapping | none | See [release](#release). The release stage is configured only when `release.github` is present. |
| `brews` | list | none | Homebrew formulae. See [brews](#brews). |
| `homebrew` | mapping | none | A single formula, same fields as one `brews` entry; appended to `brews`. |
| `brews_from` | relative path | none | A GoReleaser configuration (e.g. `.goreleaser.yaml`) whose `brews` entries are read, checked field by field and appended to `brews`. See [GoReleaser compatibility](compatibility/goreleaser.md). |
| `limits` | mapping | see [limits](#limits) | Resource bounds for archives and imports. |

`dist`, `manifest`, `builds[].dir`, `release.github.notes_file` and
`builds[].goreleaser.artifacts` must be clean relative paths: no absolute
paths, `..`, `.` or empty segments, backslashes or control characters.

## project

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | Project name, `^[A-Za-z0-9][A-Za-z0-9._+-]*$`. Template variables `.Project` and `.ProjectName`. Default formula name. |
| `description` | string | empty | Default `brews[].description`. |
| `homepage` | string | empty | Default `brews[].homepage`. |
| `license` | string | empty | Default `brews[].license` (an SPDX expression). |

## builds

Each entry produces, or imports, one executable per target and records it in
the manifest as a `binary` artifact. Exactly one provider is selected per
entry: `command` (the default), `existing`, or `goreleaser`.

### Fields shared by every provider

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `id` | string | `default` when there is a single build | Unique id, `^[A-Za-z0-9][A-Za-z0-9._-]*$`. Referenced by `archives[].builds`. Required when there are several builds. |
| `targets` | list of `{os, arch, arm}` | none | Explicit targets. `os` and `arch` match `^[a-z0-9][a-z0-9_]*$`; `arm` is digits or empty. |
| `matrix` | `{os: [...], arch: [...], exclude: [{os, arch}]}` | none | Cross product of `os` × `arch`, minus `exclude` entries (an empty `os` or `arch` in an exclude matches any). Both lists must be non-empty. |

`command` and `existing` builds need `targets` or `matrix` (or both: explicit
targets come first, then the matrix, in declaration order). A target declared
twice is an error. `goreleaser` builds must not declare either: the targets
come from GoReleaser's `artifacts.json`.

Every recorded file must be a non-empty regular file inside the project root
and at most `limits.max_file_size`. A symbolic link is accepted only when it
resolves inside the root. Size and SHA-256 are always computed from the bytes
on disk.

### command provider

Runs a command once per target.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `command` | list of templates, or a template string | required | A list is executed directly (no shell). A string is run with `sh -c` (Unix) or `cmd.exe /d /s /c` (Windows). |
| `output` | template | required | Project-relative path the command must create for the target. Must differ between targets (include `{{ .OS }}`, `{{ .Arch }}` and, for ARM, `{{ .Arm }}`). |
| `dir` | template | project root | Working directory, relative to the root; must exist and be a directory inside the root. |
| `env` | map of name → template | none | Variables added to the command's environment. |
| `timeout` | duration (`90s`, `10m`) | `30m` | Per-target limit. On expiry the command and its process group are killed. |

Behavior:

- Hephaestus **does not cross-compile**. It tells the command which target to
  build and where to write it; producing a correct executable is the
  command's job. Each run receives `HEPHAESTUS_OS`, `HEPHAESTUS_ARCH`,
  `HEPHAESTUS_ARM` and `HEPHAESTUS_OUTPUT` (the absolute output path).
- The environment is the process environment **without credentials**, then
  top-level `env`, then the build's `env` (sorted by name), then the
  `HEPHAESTUS_*` variables; a later value for the same name wins. Removed
  from the inherited environment: `GITHUB_TOKEN`, `HOMEBREW_TAP_GITHUB_TOKEN`,
  the variables named by `--token-env` / `--tap-token-env`, the variables
  `brews[].repository.token` reads, `GH_TOKEN`, `GITHUB_PAT`,
  `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`, `ACTIONS_RUNTIME_TOKEN`,
  `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, `ACTIONS_ID_TOKEN_REQUEST_URL`, and every
  variable whose name looks like a credential (as for `.Env` in
  [Templates](#variables)). A build that really needs one opts in
  through its `env`, e.g. `GITHUB_TOKEN: "{{ .Env.GITHUB_TOKEN }}"` (build
  `env` values are the one place besides `repository.token` where `.Env`
  sees credential variables). Build commands run third-party code
  (compilers, dependencies, code generators), so they should not hold
  publishing credentials. This only controls the commands' environment:
  code running as the same user may still read the environment of the
  processes around it, so build and publish in separate jobs when it
  matters ([github-action.md](github-action.md#separate-build-and-publish-jobs)).
- A program name without a path separator is looked up in the `PATH` of the
  command's own environment; relative `PATH` entries are ignored.
- Every target is planned (templates rendered, paths validated) before any
  command runs. A previous output file is removed before its target runs, and
  its parent directory is created. Standard input is the null device.
- A command that exits successfully without creating `output` fails the
  build. A failure quotes the last 4 KiB of the command's standard error.
- Targets run sequentially, in declaration order.

### existing provider

Takes prebuilt files.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `existing.path` | template | required | Project-relative path of the file for each target. Must resolve to a different path per target. |
| `existing.kind` | `binary` \| `archive` | `binary` | `binary`: an executable, recorded as a `binary` artifact. `archive`: a `.tar.gz`, `.tgz` or `.zip`, inspected and recorded as a release-ready `archive` artifact. |
| `existing.allow_sensitive` | bool | `false` | With `kind: archive`: accept members that look like credentials (by name or content, as in [archives.md](archives.md#security)). |

An imported archive's file name may contain only `A-Z a-z 0-9 . _ -` (GitHub
renames other characters). Its members are inspected without extraction and
the archive is rejected for absolute or `..` member names, links, device
files, setuid/setgid/sticky bits, duplicate or case-colliding paths, or sizes
above the limits. Members with an executable bit are recorded as its
executables.

### goreleaser provider

Imports GoReleaser output from `artifacts.json`.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `goreleaser.artifacts` | relative path | `dist/artifacts.json` | GoReleaser's artifact list. |
| `goreleaser.ids` | list of strings | all | Keep only artifacts whose `extra.ID` is listed (build ids for binaries, archive ids for archives). |
| `goreleaser.archives` | bool | `false` | Also import artifacts of type `Archive` (tar.gz, tgz or zip only) as release-ready archives. |
| `goreleaser.goamd64` | `v1`–`v4` | `v1` | For `amd64`, import only artifacts of this level. |
| `goreleaser.allow_sensitive` | bool | `false` | With `archives: true`: accept archive members that look like credentials. |

Mapping rules: `type: Binary` artifacts become binaries (GoReleaser's
`format: binary` copies are skipped); `goos`/`goarch`/`goarm` become
`os`/`arch`/`arm`; a darwin universal binary keeps `arch: all`. Other variant
fields (`goarm64`, `go386`, `gomips`, ...) are not read, so several variants
of one target collide and must be narrowed with `goreleaser.ids`. An
`extra.Checksum` (sha256, sha512, sha384, sha224, sha1, md5 or crc32) is
re-computed and a mismatch is an error. See
[compatibility/goreleaser.md](compatibility/goreleaser.md).

## archives

Each entry is instantiated once per target. The complete reference, including
path semantics, permissions and determinism, is [archives.md](archives.md).

| Field | Type | Default |
| --- | --- | --- |
| `id` | string | `default` when there is one archive; required otherwise |
| `builds` | list of build ids | all builds |
| `targets` | list of `{os, arch, arm}` | from the manifest's binaries |
| `name` | template | `{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}` |
| `format` | `tar.gz` \| `zip` | `tar.gz` |
| `format_overrides` | list of `{os, format}` | none |
| `wrap_in_directory` | bool or template | `false` |
| `files` | list (string or mapping) | the target's binaries; an explicit empty list is an error |
| `mtime` | template (Unix seconds or RFC 3339) | `$SOURCE_DATE_EPOCH`, else `1980-01-01T00:00:00Z` |
| `symlinks` | `follow` \| `reject` | `follow` |

File entries accept `src`, `dst`, `exclude`, `mode`, `required` (default
`true`), `strip_parent`, `when: {os, arch}`, `info: {owner, group, mode,
mtime}` and `allow_sensitive`. `mode` (or `info.mode`) must be an octal
string such as `"0644"`, readable by the owner, and must not be group- or
world-writable or set setuid, setgid or sticky bits.

## checksum

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `disable` | bool | `false` | Skip the checksum stage. |
| `name` | template | `{{ .Project }}_{{ .Version }}_checksums.txt` | File name, written to `<dist>/`. Must render to a plain file name. |
| `algorithm` | string | `sha256` | Accepted for GoReleaser compatibility; any value other than `sha256` is an error. |

The file lists every `archive` artifact in the manifest as
`<sha256>  <name>` lines (two spaces, `sha256sum -c` compatible), sorted by
name. Each archive is re-hashed from disk and compared with the manifest
first; a mismatch fails the stage. Binaries are not listed.

## release

Only `release.github` exists. The release stage is part of the default run
when the section is present and not disabled.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enabled` | bool | `true` when the section is present | Set `false` to keep the section but skip the stage. |
| `owner` | string | `--repo`, else `$GITHUB_REPOSITORY` | Repository owner. Not a template. |
| `name` | string | as `owner` | Repository name. Not a template. |
| `name_template` | template | the tag | Release title. |
| `draft` | bool | `false` | Leave the release as a draft after uploading. A published release is never turned back into a draft. |
| `prerelease` | `auto` \| `true` \| `false` | `auto` | `auto`: a prerelease when the tag is a semantic version with a pre-release part (`v1.2.0-rc.1`). |
| `make_latest` | `true` \| `false` \| `legacy` | GitHub's default | Sent only when the release is published by this run and is not a prerelease. |
| `notes` | template | none | Release body. |
| `notes_file` | relative path | none | Release body read from a file inside the project root (at most 1 MiB, UTF-8; a link out of the root is refused). Not a template. Mutually exclusive with `notes`. |
| `generate_notes` | bool | `false` | When the release is **created** and neither `notes` nor `notes_file` is set, ask GitHub to generate the notes. The body of an existing release is never changed by this setting. |
| `target_commitish` | string | the commit being released (`--commit`, else `$GITHUB_SHA`; used only when it is a full SHA) | Where GitHub creates the tag when it does not exist yet; ignored by GitHub for an existing tag. Without either, a warning says the tag will be created at the default branch's HEAD. Pushing the tag before releasing is recommended. |
| `replace_existing_assets` | bool | `false` | Replace a same-named asset whose content differs. Identical assets are always left alone. |
| `include` | list of `archive`, `checksum`, `binary` | `[archive, checksum]` | Manifest kinds uploaded as assets. |
| `api_url` | URL | `$GITHUB_API_URL`, else `https://api.github.com` | GitHub Enterprise Server: `https://HOST/api/v3`. Must be `https` (`http` only for a loopback address). The tap stage uses the same API URL. |
| `upload_url` | URL | derived from the API URL: `https://uploads.github.com` for github.com, `https://uploads.SUB.ghe.com` for `https://api.SUB.ghe.com`, `https://HOST/api/uploads` for GitHub Enterprise Server | Asset upload endpoint. Same scheme rule as `api_url`. |

Notes: the release body is at most 125,000 characters. With `include:
[binary]`, binary file names become asset names, so binaries with the same
name for several targets collide and fail the stage. `notes` and
`notes_file` are applied on every run (the body is updated when it differs);
without either, an existing body is left as it is. Publication behavior is
described in the [README](../README.md#github-releases).

## brews

Each entry generates one Homebrew formula from the manifest's archives, and
optionally publishes it to a tap. Field names follow GoReleaser's `brews`;
the field-by-field status is in
[compatibility/goreleaser.md](compatibility/goreleaser.md).

```yaml
brews:
  - name: mycli
    description: Friendly tool for managing widgets
    homepage: https://github.com/acme/mycli
    license: MIT
    directory: Formula
    install: |
      bin.install "mycli"
    test: |
      assert_match version.to_s, shell_output("#{bin}/mycli --version")
    repository:
      owner: acme
      name: homebrew-tap
      pull_request:
        enabled: true
```

### Formula content

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | `project.name` | Formula name: `^[a-z0-9][a-z0-9+_.-]*(@[0-9][0-9.]*)?$`, starting with a letter. Determines the file (`<name>.rb`) and the Ruby class name. Unique across `brews`. |
| `ids` | list of archive ids | all archives | Restrict to archives of these `archives[].id`. Each id must be declared in `archives`. |
| `goarm` | string | — | Accepted; ignored with a warning (32-bit ARM is not supported by Homebrew). |
| `goamd64` | string | — | Accepted; a value other than `v1` is ignored with a warning (the manifest has no amd64 level). |
| `url_template` | template | see below | Download URL per archive. Target and `.ArtifactName`/`.ArtifactPath`/`.ArtifactExt` variables are set. |
| `url_headers` | list of templates | none | `headers:` of each `url` stanza; each must be a single line. |
| `download_strategy` | Ruby class name or `:symbol` | none | `using:` of each `url` stanza. |
| `custom_require` | path | none | `require_relative "<value>"` at the top; characters `A-Z a-z 0-9 _ . / -` only. |
| `homepage` | template | `project.homepage` | Must render to an http(s) URL; http produces a warning. Empty produces a warning. |
| `description` | template | `project.description` | `desc`. Homebrew style problems (length, leading article, the formula name, trailing period, ...) are warnings. |
| `license` | SPDX expression | `project.license` | `MIT`, `MIT OR Apache-2.0`, `(A AND B) OR C`; rendered as `license any_of:`/`all_of:`. Empty produces a warning. |
| `version` | template | the release version | Formula version. Must not start with `v`; must not end in `_<number>`. Omitted from the formula when every URL is a github.com release download URL whose tag is the version. |
| `dependencies` | list of names or `{name, type, os, version}` | none | `depends_on`. `type`: `build`, `test`, `optional`, `recommended`. `os`: `mac` or `linux` (placed inside `on_macos`/`on_linux`; skipped with a warning when the formula has no archive for that OS). `version` is accepted and ignored with a warning. Names may be tap-qualified (`owner/tap/name`). |
| `conflicts` | list of formula names | none | `conflicts_with`. |
| `install` | template (Ruby) | `bin.install` of every executable of every archive; one present in only some archives gets a condition (`if OS.mac?`) | Body of the single `def install`, rendered for every archive. Code that renders differently per archive is wrapped in `if OS.mac? && Hardware::CPU.arm?`-style conditions selecting the archives it belongs to. |
| `extra_install` | template (Ruby) | none | Appended to `install`. Same rule. |
| `post_install` | template (Ruby) | none | `def post_install` (a warning notes its deprecation in Homebrew). |
| `caveats` | template | none | `def caveats` with a `<<~EOS` heredoc; the text is escaped. |
| `custom_block` | template (Ruby) | none | Inserted into the class body. |
| `service` | template (Ruby) | none | `service do ... end`. |
| `test` | template (Ruby) | an `assert_predicate bin/"<exe>", :executable?` per installed executable | `test do ... end`. |
| `plist` | — | — | Rejected: Homebrew removed plist blocks; use `service`. |
| `output` | template | `<dist>/homebrew/<directory>/<name>.rb` | Local path of the generated formula; must end in `.rb`. |
| `verify` | bool | `true` | Check that every path referenced by a statically analyzable `bin.install`, `man1.install`, ... call exists in every archive the call applies to. |

Default download URL, in order: `url_template`; else the archive's
`download_url` recorded by the release stage (only for a published release);
else `https://github.com/<owner>/<repo>/releases/download/<tag>/<name>`,
with the repository from `--repo`, `release.github.owner/name` or
`$GITHUB_REPOSITORY`. URLs must be `https` without credentials (`file://` is
allowed only with `HEPHAESTUS_ALLOW_FILE_URLS=1`, for tests).

Archive selection: only `darwin` and `linux` archives with arch `amd64` or
`arm64` are used; other architectures are skipped with a warning. Two
archives for the same OS and arch are an error (narrow with `ids`). At least
one archive must remain.

Ruby code fields (`install`, `extra_install`, `post_install`, `custom_block`,
`service`, `test`) are trusted configuration, embedded as code. Template
values substituted into them must consist of `A-Z a-z 0-9 . _ + - / @ : ~ = ,
% & ?` only, and the code may not contain control or invisible characters,
`__END__`, `=begin`/`=end`, plain `<<ID` heredocs or unterminated heredocs.

### Publication (tap stage)

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `repository.owner` | template | required for the tap stage | Tap owner. |
| `repository.name` | template | required for the tap stage | Tap repository (e.g. `homebrew-tap`). |
| `repository.branch` | template | PR mode: `{{ .ProjectName }}-{{ .Version }}`; direct mode: the default branch | Branch the commit is written to. |
| `repository.token` | template | `$HOMEBREW_TAP_GITHUB_TOKEN` | Token for this tap only, e.g. `{{ .Env.ACME_TAP_TOKEN }}`. Rendered against the complete environment (credential variables included) and masked in the log. Never write a token literally. |
| `repository.token_type` | `github` | `github` | Any other value is an error. |
| `repository.git` | — | — | Rejected: publishing over plain git is not supported. |
| `repository.pull_request.enabled` | bool | **`true`** (`false` for entries read through `brews_from`, as in GoReleaser) | Pull request mode. `false` selects direct mode (commit to `repository.branch` or the default branch). `hephaestus check` prints each formula's mode. |
| `repository.pull_request.draft` | bool | `false` | Open the pull request as a draft. |
| `repository.pull_request.base` | `{owner, name, branch}` or a branch string | the tap repository, its default branch | Pull request target; `owner` and `name` together (e.g. the upstream of a fork). |
| `repository.pull_request.check_boxes` | bool | — | Accepted with a warning; no effect. |
| `directory` | template | tap root | Directory of the formula inside the tap (commonly `Formula`). Before writing, the tap stage checks it against the tap the way Homebrew reads it (the first of `Formula/`, `HomebrewFormula/` and the root that exists is the only formula directory; at the root only top-level `*.rb` files count) and refuses a formula Homebrew would not find, or a new `Formula/` that would hide formulae kept at the root. |
| `folder` | — | — | Deprecated GoReleaser name of `directory`; converted with a warning. |
| `tap` | — | — | Deprecated GoReleaser name of `repository`; converted with a warning. |
| `commit_msg_template` | template | `Brew formula update for {{ .ProjectName }} version {{ .Tag }}` | Commit message (and pull request title). |
| `commit_author.name` / `.email` | templates | the token's identity | Both or neither. |
| `commit_author.signing` | — | — | Rejected: commits are created through the GitHub API. |
| `skip_upload` | `true` \| `false` \| `auto` | `false` | `true`: generate but never publish. `auto`: skip prereleases. |
| `allow_downgrade` | bool | `false` | Before writing, the version of the formula already in the tap (its `version` line, else a semantic version in its URL path; in pull request mode, on the base branch) is compared with the one being published. When the tap's is newer (for example an older tag's workflow was re-run), nothing is written and the formula is reported as skipped, unless this is `true`. Versions that are not semantic versions cannot be compared: the formula is updated with a warning. |

## limits

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `max_file_size` | size | `2GiB` | Largest single file: build outputs, archive sources and members of imported archives. |
| `max_total_size` | size | `4GiB` | Largest total uncompressed content of one archive. |
| `max_files` | integer | `10000` | Most entries in one archive; also bounds glob expansion. |

Sizes are a number of bytes or a number with `B`, `KiB`, `MiB`, `GiB` or
`TiB`. Values must not be negative.

```yaml
limits:
  max_file_size: 512MiB
  max_total_size: 1GiB
  max_files: 2000
```

## Templates

Values marked *template* are Go `text/template` expressions. Rendering is
strict: an unknown field, an unknown function or a missing `.Env` key is an
error, and every template in the file is test-rendered during validation so
typos fail before any work starts (environment variables need not exist at
that point). A rendered value is at most 64 KiB. A value without `{{` is
used as is.

### Variables

| Variable | Value |
| --- | --- |
| `.Project` | `project.name` |
| `.ProjectName` | alias of `.Project` (GoReleaser) |
| `.Version` | the tag without a leading `v` (`1.2.3`); `0.0.0-SNAPSHOT` with `--snapshot` |
| `.Tag` | the tag as given (`v1.2.3`); empty with `--snapshot` |
| `.RawVersion` | the tag as given |
| `.Major`, `.Minor`, `.Patch`, `.Prerelease` | parts of a semantic-version tag; empty when the tag is not a semantic version |
| `.Commit`, `.FullCommit` | `--commit`, else `$GITHUB_SHA`; may be empty |
| `.ShortCommit` | the first 7 characters of `.Commit` |
| `.OS`, `.Os` | the target OS; empty outside a per-target value |
| `.Arch` | the target architecture |
| `.Arm` | the ARM version, if any |
| `.Ext` | `.exe` when the OS is `windows`, else empty |
| `.ArtifactName`, `.ArtifactPath`, `.ArtifactExt` | the archive a value is rendered for (`brews[].url_template`, `install`, `extra_install`); empty elsewhere |
| `.Env.NAME` | the process environment, overridden by top-level `env`. The token variables, the variables `brews[].repository.token` reads, and variables whose name looks like a credential (contains `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `PASSPHRASE`, `CREDENTIAL`, `PRIVATE_KEY`, `API_KEY`, `APIKEY` or `ACCESS_KEY`, or has a `_`-separated segment `PAT`, `AUTH`, `KEY`, `PASS` or `PWD`, ignoring case) are not visible unless they are declared in top-level `env` — except in `builds[].env` values and `brews[].repository.token`, which see the complete environment. A rendered value that contains a token is an error. |

Per-target variables are set in `builds` (`command`, `output`, `dir`, `env`,
`existing.path`), in `archives` and in the formula fields rendered per
archive (`url_template`, `url_headers`, `install`, `extra_install`).

A tag must be 1–128 characters of letters, digits, `.`, `_`, `+` and `-`,
without `..`. It is taken from `--tag`, else from `$GITHUB_REF_NAME` when
`$GITHUB_REF_TYPE` is `tag`.

### Functions

| Function | Effect |
| --- | --- |
| `lower`, `tolower` | lower case |
| `upper`, `toupper` | upper case |
| `title` | upper-cases the first byte only |
| `trim` | trims surrounding white space |
| `trimprefix s prefix`, `trimsuffix s suffix` | remove a prefix or suffix |
| `replace s old new` | replace every occurrence |
| `urlPathEscape` | escape one URL path segment |

Go's built-in template functions (`printf`, `eq`, `ne`, `and`, `or`, `not`,
`len`, `index`, ...) and actions (`if`, `with`, `range`) are available.
Nothing else is: there is no file, environment or time access beyond the
variables above.

```yaml
archives:
  - id: default
    name: "{{ .Project }}-{{ .Version }}-{{ .OS }}-{{ .Arch }}"
    wrap_in_directory: true
```
