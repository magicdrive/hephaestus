# GoReleaser compatibility

Hephaestus reads GoReleaser's `brews` field names so that an existing Homebrew
configuration can be copied into `hephaestus.yaml`, and it can import
GoReleaser's build output (`dist/artifacts.json`). It is not a GoReleaser
replacement for everything else: archives, release and templates use their
own, smaller schema.

This page is the field-by-field record. The reference GoReleaser version is
the v2 OSS schema (v2.19 / main at the time of writing). Every status below
is taken from the implementation (`internal/config`, `internal/homebrew`,
`internal/tap`, `internal/compat/goreleaser`) and names the tests that pin
it. "—" in the tests column means no test pins the behavior yet.

Status values:

| Status | Meaning |
| --- | --- |
| **Supported** | Same meaning as GoReleaser. Differences, if any, are listed. |
| **Supported, differs** | Accepted and used, with a deliberate behavior difference. |
| **Accepted, no effect** | Accepted with a warning; does nothing. |
| **Rejected** | A configuration error with an explanatory message. Never silently ignored. |
| **Not supported** | The key is unknown to Hephaestus: an `unknown field` error. |

## Two ways to read `brews`

1. **`hephaestus.yaml`** (`internal/config`) — what the CLI and the GitHub
   Action use. Paste GoReleaser's `brews` list under `brews:`. Unknown keys
   are errors (with a "did you mean" suggestion).
2. **The GoReleaser adapter** (`internal/compat/goreleaser`) — set
   `brews_from: .goreleaser.yaml` in `hephaestus.yaml` and the `brews` list
   is read from the GoReleaser file itself, without copying. Every key is
   classified (supported, Pro-only, removed in v2, unsupported, no-op), and
   templates are checked against what Hephaestus can render; the resulting
   formulas are appended to `brews` and validated with the rest of the
   configuration. GoReleaser's own deprecation of `brews` is reported as a
   warning. Where Hephaestus's own defaults differ, entries read this way
   get **GoReleaser's** defaults, so adopting `brews_from` does not change
   the formula's name, download URL or publishing mode: `name` defaults to
   `project_name`, `repository.pull_request.enabled` to `false`, and
   `url_template` to the `release.github` repository on
   `github_urls.download`. Tested by `TestBrewsFromGoReleaser`,
   `TestGoReleaserDefaults` and `TestLoadBrewsValid`.

The two agree except where the table says "adapter:".

## `brews` field matrix

| GoReleaser field | Type | GoReleaser meaning | Hephaestus | Behavior difference | Tests |
| --- | --- | --- | --- | --- | --- |
| `name` | string | Formula name; file `<name>.rb`; class name | Supported, differs | Not a template. Must match `^[a-z0-9][a-z0-9+_.-]*(@[0-9][0-9.]*)?$` and start with a letter (Homebrew cannot load a class name starting with a digit). Default in `hephaestus.yaml`: `project.name`. adapter: GoReleaser's `project_name`, as GoReleaser does, so `brews_from` never renames a formula; when `project_name` and `project.name` differ, a warning says that `{{ .ProjectName }}` in templates (commit message, pull request branch) is still `project.name`. | `TestClassName`, `FuzzClassName`, `TestValidateBrewNameStartsWithLetter`, `TestGoReleaserDefaults`, `TestBrewsFromGoReleaser` |
| `alternative_names` | []string | Extra versioned formulae (Pro) | Not supported | adapter: rejected as Pro-only. | `TestLoadBrewsPro` |
| `app` | string | DMG app (Pro) | Not supported | adapter: rejected as Pro-only. | `TestLoadBrewsPro` |
| `templated_files` | — | Pro | Not supported | adapter: rejected as Pro-only. | `TestLoadBrewsPro` |
| `ids` | []string | Archive or binary ids to include | Supported, differs | Ids of Hephaestus `archives[]` entries, or ids of builds that import archives (`goreleaser` with `archives: true`, `existing` with `kind: archive`) — imported archives carry the build id. Uploadable binaries (`format: binary`) are never used. | `TestSelectArchives`, `TestBrewIDsMayNameImportingBuilds` |
| `goarm` | string | GOARM to use for 32-bit ARM | Accepted, no effect | 32-bit ARM archives are always skipped (Homebrew does not support them); a warning is printed. | — |
| `goamd64` | string | GOAMD64 level to use | Accepted, no effect | The manifest has no amd64 level; a value other than `v1` produces a warning. Select the level when importing (`builds[].goreleaser.goamd64`). | — |
| `url_template` | template | Download URL | Supported, differs | Default in `hephaestus.yaml`: the `download_url` the release stage recorded, else `https://github.com/<owner>/<repo>/releases/download/<tag>/<name>` for the release repository. adapter: GoReleaser's default, `<github_urls.download>/<release.github.owner>/<release.github.name>/releases/download/{{ urlPathEscape .Tag }}/{{ .ArtifactName }}`, whenever `.goreleaser.yaml` sets `release.github` (a distribution repository other than the source) or `github_urls.download` (GitHub Enterprise); `github_urls.download` without `release.github` is an error asking for `url_template`, because GoReleaser takes that repository from the git remote. Must be `https` without credentials. | `TestGenerateURLs`, `TestGolden` (`url_template`, `non_github_url`), `TestGoReleaserDefaults`, `TestGoReleaserCompatibility` |
| `url_headers` | []string | `headers:` of `url` | Supported | Rendered per archive; each header must be one line; values are escaped Ruby strings. | `TestGolden` (`custom`) |
| `download_strategy` | string | `using:` of `url` | Supported, differs | Not a template; must be a Ruby class name (`A::B`) or a `:symbol`. | `TestGolden` (`custom`) |
| `custom_require` | string | `require_relative` | Supported, differs | Not a template; characters `A-Z a-z 0-9 _ . / -` only. | `TestGolden` (`custom`) |
| `commit_author.name` / `.email` | string | Commit author | Supported, differs | **No default author**: when unset, the commit belongs to the token's identity (GoReleaser defaults to `goreleaserbot <bot@goreleaser.com>`). Name and email must be set together. | `TestResolvePublishDirect`, `TestResolvePublishErrors`, `TestPullRequestNew` |
| `commit_author.signing.*` | mapping | GPG/SSH signing (git repositories) | Rejected | Commits go through the GitHub API; GitHub signs API commits made with a GitHub App token. | `TestLoadBrewsUnsupported` |
| `commit_author.use_github_app_token` | bool | Committer omitted for App-signed commits | Not supported | `hephaestus.yaml`: unknown field. adapter: accepted with a no-op warning. | `TestLoadBrewsUnsupported` |
| `commit_msg_template` | template | Commit message | Supported | Same default (`Brew formula update for {{ .ProjectName }} version {{ .Tag }}`). Also used as the pull request title. | `TestResolvePublishDirect`, `TestResolvePublishErrors` |
| `directory` | string | Directory in the tap | Supported, differs | Hephaestus also renders it as a template (GoReleaser uses it verbatim). The local copy is written to `<dist>/homebrew/<directory>/<name>.rb`. Hephaestus refuses to publish where Homebrew would not find the formula, or where a new `Formula/` would hide formulae at the tap root (GoReleaser writes it anyway). | `TestGenerateDirectoryAndOutput`, `TestGenerateDirectoryTemplate`, `TestResolvePublishDirect`, `TestFormulaDirectory`, `TestFormulaDirectoryBlocksPublishing` |
| `folder` | string | Removed in GoReleaser v2 (old `directory`) | Supported, differs | Converted to `directory` with a deprecation warning. | `TestDeprecatedGoReleaserNames`, `TestLoadBrewsRemoved` |
| `caveats` | string | `def caveats` | Supported, differs | Rendered as a template, then written as an escaped `<<~EOS` heredoc: `#{...}` is shown literally (GoReleaser's heredoc interpolates it, so Ruby evaluates it at install time). adapter: `#{` in caveats is reported with a warning. ANSI escapes produce a warning. | `TestGolden` (`custom`), `TestRubyInjection`, `TestLoadBrewsInline`, `TestGoReleaserCompatibility` |
| `homepage` | string | `homepage` | Supported, differs | Default `project.homepage` (GoReleaser OSS: empty). Must be an http(s) URL. | `TestGenerateDefaults`, `TestRubyInjection` |
| `description` | string | `desc` | Supported, differs | Default `project.description`. Homebrew `desc` style rules produce warnings. | `TestLintDesc`, `TestGenerateLicenseAndDescWarnings` |
| `license` | string | `license` | Supported, differs | Default `project.license`. Not a template. Parsed as an SPDX expression and written in Homebrew's `any_of:`/`all_of:` form; a single unknown identifier (`Proprietary`) is written as is, like GoReleaser. `:public_domain` and `:cannot_represent` are written as Homebrew symbols. Free text (`Apache License 2.0`) is an error with a hint (GoReleaser writes it verbatim; Homebrew's audit rejects it). | `TestLicenseDSL`, `TestLicenseRubySyntax`, `TestGenerateLicenseAndDescWarnings`, `TestLicenseSymbolsAndFreeText` |
| `skip_upload` | string | `true` / `auto` | Supported, differs | Not a template: `true`, `false` or `auto` only. | `TestResolvePublishSkip`, `TestSkipUpload` |
| `custom_block` | string | Inserted into the class | Supported | Rendered; checked as a Ruby code block (see [Templates in code](#templates-in-ruby-code)). | `TestGolden` (`custom`), `TestRubyInjectionInCodeFields` |
| `dependencies[]` | string or `{name, type, version, os}` | `depends_on` | Supported, differs | Not templates. Sorted build, test, runtime, recommended, optional (GoReleaser: the same groups). `os` dependencies go inside `on_macos`/`on_linux` instead of `if OS.mac?`, and are skipped with a warning when the formula has no archive for that OS. `version` is **ignored with a warning** (GoReleaser writes `=> "version"`, which Homebrew does not treat as a constraint). | `TestGolden` (`custom`), `TestBrewStrictSubstructures`, `TestDependencyWarnings` |
| `conflicts` | []string | `conflicts_with` | Supported, differs | Not templates; each must be a valid formula name. | `TestGolden` (`custom`) |
| `service` | string | `service do` | Supported | Code block rules apply. | `TestGolden` (`custom`) |
| `plist` | — | Removed in GoReleaser v2 | Rejected | Homebrew removed plist blocks; use `service`. | `TestRejects`, `TestLoadBrewsRemoved` |
| `test` | string | `test do` | Supported, differs | When unset, Hephaestus generates `assert_predicate bin/"<exe>", :executable?` for each installed executable (GoReleaser writes no test). | `TestGolden`, `TestGenerateDefaults` |
| `install` | string | Install code | Supported, differs | Rendered for every archive, as GoReleaser does. Hephaestus writes **one** `def install` (GoReleaser: `define_method(:install)` per platform): code that renders the same everywhere is written once, and code that differs is wrapped in `if OS.mac? && Hardware::CPU.arm?`-style conditions selecting the archives it was rendered for. Default: `bin.install` of every executable in every archive, as GoReleaser; an executable present in only some archives is installed under a condition selecting them (`bin.install "helper" if OS.mac?`). Every path is checked against the archive it applies to. | `TestInstallPerTargetTemplate`, `TestDefaultInstallEveryExecutable`, `TestPlatformConds`, `TestGolden` (`partial_executables`, `per_target_install`), `TestGoReleaserCompatibility` |
| `extra_install` | string | Appended to install | Supported, differs | Same per-archive rule, written after `install`. | `TestExtraInstallPerTarget`, `TestGolden` (`wrap_in_directory`, `per_target_install`) |
| `post_install` | string | `def post_install` | Supported | A warning notes that Homebrew deprecated `def post_install`. | `TestGolden` (`custom`) |
| `repository.owner` / `.name` | string | Tap repository | Supported | Required for the tap stage, not for the formula stage. | `TestResolvePublishDefaults`, `TestResolvePublishErrors` |
| `repository.branch` | string | Branch to push to | Supported | Same default in pull request mode (`{{ .ProjectName }}-{{ .Version }}`, GoReleaser v2.17+); direct mode defaults to the tap's default branch. | `TestResolvePublishPullRequest`, `TestDirectCreatesNewFileOnConfiguredBranch` |
| `repository.token` | string | Token for this repository | Supported | Rendered when publishing. | `TestRepositoryTokenUsesClientFor` |
| `repository.token_type` | string | `github`/`gitlab`/`gitea` (Pro) | Supported, differs | `hephaestus.yaml`: only `github` (or empty). adapter: rejected as Pro-only. | `TestResolvePublishErrors`, `TestLoadBrewsPro` |
| `repository.git.*` | mapping | Publish over plain git/SSH | Rejected | Hephaestus publishes only through the GitHub API. | `TestLoadBrewsUnsupported` |
| `repository.pull_request.enabled` | bool | Open a pull request | Supported, differs | Default in `hephaestus.yaml`: **`true`** (GoReleaser: `false`). adapter: GoReleaser's default `false` (a direct commit), so `brews_from` does not change how a formula is published. `hephaestus check` prints each formula's publishing mode and the GoReleaser defaults applied; the tap stage's dry run prints the exact writes. In pull request mode `repository.branch` is the head branch: the same branch as `pull_request.base` is an error, and a head branch named `main`, `master`, `trunk` or `develop` without a base is warned about (the tap stage refuses head = base before writing). | `TestResolvePublishDefaults`, `TestResolvePublishDirect`, `TestGoReleaserDefaults`, `TestBrewsFromGoReleaser`, `TestPullRequestBranchConflicts` |
| `repository.pull_request.draft` | bool | Draft pull request | Supported | | `TestResolvePublishPullRequest`, `TestPullRequestNew` |
| `repository.pull_request.base` | `{owner, name, branch}` or string | Pull request target | Supported | A bare string is the branch, as in GoReleaser. `owner` and `name` must be set together. | `TestResolvePublishPullRequest`, `TestCrossRepositoryPullRequest`, `TestBrewStrictSubstructures` |
| `repository.pull_request.body` | string | Pull request body | Not supported | The body is fixed ("Update the <name> formula to version <v>. Opened by Hephaestus."). adapter: rejected as unsupported. | `TestLoadBrewsUnsupported` |
| `repository.pull_request.token` | string | Separate PR token | Not supported | Use `repository.token`. adapter: rejected as unsupported. | `TestLoadBrewsUnsupported` |
| `repository.pull_request.check_boxes` | bool | Fill PR template boxes (Pro) | Accepted, no effect | `hephaestus.yaml`: warning. adapter: rejected as Pro-only. | `TestLoadBrewsPro` |
| `tap` | mapping | Removed in GoReleaser v2 (old `repository`) | Supported, differs | Converted to `repository` with a deprecation warning. | `TestDeprecatedGoReleaserNames`, `TestLoadBrewsRemoved` |

Hephaestus-only `brews` fields: `version` (formula version override;
`TestGenerateVersion`), `output` (local path; `TestGenerateDirectoryAndOutput`)
`verify` (install-path verification, default `true`; `TestVerifyFormula`,
`TestVerifyMissing`) and `allow_downgrade` (let the tap stage replace a newer
formula with an older version; default `false`).

GoReleaser has deprecated `brews` in favor of `homebrew_casks` (soft in v2.10,
fully in v2.16) but still runs it in v2. Hephaestus keeps generating
formulae; the adapter prints a warning about the deprecation and ignores
`homebrew_casks` with a warning (`TestLoadBrewsValid`, `TestLoadBrewsInline`).

### Generated formula: structural differences

The configuration is GoReleaser-compatible; the **output follows current
Homebrew style** rather than GoReleaser's template (golden files in
`internal/homebrew/testdata/*.golden`, `TestGolden`, `TestGoldenRubySyntax`,
and a real `brew audit --strict` / `install` / `test` in
`scripts/homebrew-e2e.sh`):

| Aspect | GoReleaser | Hephaestus |
| --- | --- | --- |
| Platform blocks | `on_macos`/`on_linux` with `if Hardware::CPU.intel?` | `on_macos`/`on_linux` containing `on_arm`/`on_intel` |
| Install | `define_method(:install)` per platform | One `def install`; code that differs per archive (templates, executables present in only some archives) is wrapped in `if OS.mac? && Hardware::CPU.arm?`-style conditions, so each platform installs the same files as with GoReleaser |
| 32-bit ARM, 386 | `arm` supported via `goarm` | Skipped with a warning |
| `is_64_bit?` checks | Emitted for Linux | Never emitted |
| macOS universal binaries (`goarch: all`) | Supported | Skipped with a warning (only `amd64` and `arm64`) |
| macOS without arm64 | The amd64 package serves every Mac (Rosetta 2), with an Apple Silicon caveat that a user `caveats` overrides | Same: `on_arm` and `on_intel` both name the amd64 archive, plus a generation warning; no caveat |
| Linux without arm64, or any OS without amd64 | The missing CPU has no `url` (the formula fails to load there) | `depends_on arch: :x86_64` / `:arm64`, so Homebrew explains why, plus a warning |
| `version` | Always written | Omitted when Homebrew derives the same version from github.com release URLs |
| Dependency `os` | `if OS.mac?` modifier | Inside `on_macos`/`on_linux` |
| License expressions | Written verbatim | SPDX parsed into `any_of:`/`all_of:` |
| Two-pass templating | The whole rendered formula is templated a second time | No second pass; only the fields marked as templates are rendered |
| Header comment | "generated by GoReleaser" | "generated by Hephaestus" |
| Selection errors | No archive / several archives per OS+arch | Same two errors |

### Comparison with real GoReleaser output

`testdata/compat/goreleaser/` holds a small project, its `.goreleaser.yaml`
and what GoReleaser **v2.19.0** produced from it (formulae, `artifacts.json`,
checksums and archive listings; regenerate with
`scripts/goreleaser-compat-fixture.sh`). `TestGoReleaserCompatibility` reads
the same `.goreleaser.yaml` through the adapter, generates formulae from the
recorded archives and requires, for every platform, the same download URL,
SHA-256 and installed executables as GoReleaser's formula. The fixture covers
a distribution repository that differs from the source (`release.github`),
an executable present only in the macOS archives, a macOS amd64-only formula
and `#{...}` in caveats. The intentional differences it meets (no `version`
line, escaped caveats, a direct commit by default) are asserted explicitly.

Not compared byte for byte, by design: the formula layout and Ruby style
(above), the header comment, `is_64_bit?` checks and GoReleaser's second
template pass. Not compared at all: GoReleaser's archive bytes (they depend on
the Go toolchain; only the recorded listings and checksums are used), Pro
features, and publishing (the fixture is never published).

### Templates in Ruby code

`install`, `extra_install`, `post_install`, `custom_block`, `service` and
`test` are embedded as Ruby code, as in GoReleaser. Hephaestus additionally
requires every template value substituted into code to use only
`A-Z a-z 0-9 . _ + - / @ : ~ = , % & ?` (`TestRenderCodeValidatesValues`), and
rejects control or invisible characters, `__END__`, `=begin`/`=end`, plain
`<<ID` heredocs and unterminated heredocs (`TestCodeBlock`,
`TestRubyInjectionInCodeFields`). Data fields (`desc`, `homepage`, URLs,
headers, dependency names, caveats) are always escaped Ruby strings
(`TestRubyString`, `FuzzRubyString`, `FuzzRubyHeredoc`, `TestRubyInjection`).

### Publication differences

| Aspect | GoReleaser | Hephaestus |
| --- | --- | --- |
| Default mode | Direct push to the default branch | Pull request in `hephaestus.yaml`; direct push for entries read through `brews_from`. `hephaestus check` prints the mode of every formula |
| Identical content | Commits anyway | No commit, no pull request (`TestDirectUpdateAndNoOp`, `TestUnchangedOnBase`) |
| Existing pull request for the head branch | — | Reused (`TestPullRequestNewFileAndRerunReuses`, `TestCreatePull422ReSearches`) |
| Force push | — | Never; references are only created (`TestNoRefUpdateAPI`) |
| Concurrent writers | — | Contents API with the blob SHA as an optimistic lock, re-read and retried (`TestDirectConflictRecovery`, `TestDirectPersistentConflictFails`) |
| Formula tampered after generation | — | Refused: the file must match the manifest's SHA-256 (`TestTamperedFormula`) |

## `archives` mapping

Hephaestus archives are declarative and do not reuse GoReleaser's key names
where the meaning differs. See [archives.md](../archives.md).

| GoReleaser field | Hephaestus | Notes |
| --- | --- | --- |
| `id` | `id` | Same. |
| `ids` | `builds` | Hephaestus calls it `builds`; `ids` is an unknown field. |
| `builds` (deprecated in v2.8) | `builds` | Same name. |
| `formats` | — | Not supported; one `format` per entry. |
| `format` | `format` | Only `tar.gz` and `zip`. `tgz`, `tar.xz`, `txz`, `tar.zst`, `tzst`, `tar`, `gz`, `xz`, `binary` are rejected. |
| `zstd` | — | Not supported. |
| `name_template` | `name` | Different key. Default differs: no `.Mips` / `.Amd64` suffixes. The rendered name may contain only `A-Z a-z 0-9 . _ -`. |
| `wrap_in_directory` | `wrap_in_directory` | Same (bool or template). |
| `strip_binary_directory` | — | Not supported; place binaries explicitly with `files[].dst`. |
| `files` | `files` | **No implicit files**: GoReleaser adds `LICENSE*`, `README*`, `CHANGELOG*` when `files` is empty; Hephaestus never does. Omitting `files` packages the target's binaries; an empty list is an error. |
| `files[].src` | `files[].src` | Glob base is the pattern's literal prefix (GoReleaser: the longest common prefix of the matches). `**` matches zero or more directories (GoReleaser: one or more). A literal directory is an error (use `dir/**`). Dot files are never matched by wildcards. |
| `files[].dst` | `files[].dst` | Templated (GoReleaser: not templated). A literal `src` with a `dst` not ending in `/` is a rename, as in GoReleaser. |
| `files[].strip_parent` | `files[].strip_parent` | Same. |
| `files[].info.owner` / `.group` / `.mode` / `.mtime` | same | Supported. `mode` must be an octal string and pass the permission policy. |
| `builds_info` | — | Not supported. Binaries packaged by default get mode `0755`. |
| `format_overrides[].goos` | `format_overrides[].os` | Different key; exact OS match (GoReleaser: prefix match). |
| `format_overrides[].formats` / `.format` | `format_overrides[].format` | One format; `none` is not supported. |
| `allow_different_binary_count` | — | Not supported (not needed: `files` is explicit). |
| `meta` | — | Not supported. |
| `templated_files`, `hooks` | — | Pro; not supported. |

Hephaestus-only: `targets`, `mtime`, `symlinks`, `files[].exclude`,
`files[].mode`, `files[].required`, `files[].when`, `files[].allow_sensitive`.
Collisions (including case-only), unsafe modes and paths are errors where
GoReleaser warns or keeps the first entry.

## `release` and `checksum` mapping

| GoReleaser | Hephaestus | Notes |
| --- | --- | --- |
| `release.github.owner` / `.name` | `release.github.owner` / `.name` | Same. GitLab and Gitea are not supported. |
| `release.name_template` | `release.github.name_template` | Inside `github`. |
| `release.draft` | `release.github.draft` | Hephaestus always creates a draft first and publishes after every asset is verified; `draft: true` only means "do not publish". |
| `release.prerelease` | `release.github.prerelease` | Same values (`auto`, `true`, `false`). |
| `release.make_latest` | `release.github.make_latest` | `true`, `false`, `legacy`; applied only when this run publishes a non-prerelease. Not a template. |
| `release.target_commitish` | `release.github.target_commitish` | Default is the build commit, not the default branch. |
| `release.replace_existing_artifacts` | `release.github.replace_existing_assets` | Different key. Identical assets are never replaced. |
| `release.header` / `.footer` / `mode` / changelog | `release.github.notes`, `notes_file`, `generate_notes` | No changelog generation by Hephaestus; `generate_notes` asks GitHub to generate notes on creation. |
| `release.extra_files`, `ids`, `include_meta`, `disable`, `skip_upload`, `discussion_category_name`, `replace_existing_draft`, `use_existing_draft` | — | Not supported. An existing draft for the tag is always reused. Uploaded kinds are chosen with `release.github.include`. |
| `checksum.name_template` | `checksum.name` | Different key. |
| `checksum.algorithm` | `checksum.algorithm` | `sha256` only. |
| `checksum.disable` | `checksum.disable` | Same. |
| `checksum.split`, `ids`, `extra_files` | — | Not supported. Only archives are listed. |

## Importing `dist/artifacts.json`

A build with `goreleaser:` reads GoReleaser's artifact list
(`internal/build/goreleaser.go`; tests `TestGoReleaserBinaries`,
`TestGoReleaserArchives`, `TestGoReleaserRejects`).

| `artifacts.json` | Hephaestus manifest |
| --- | --- |
| `type: Binary` (not `extra.Format: binary`) | `kind: binary` |
| `type: Archive` (with `goreleaser.archives: true`; tar.gz, tgz or zip) | `kind: archive`, inspected like an `existing` archive |
| any other `type` | ignored |
| `name`, `path` | `name` (base name of `path`), `path` (must be inside the project root) |
| `goos`, `goarch`, `goarm` | `os`, `arch`, `arm` (`arm` only for `goarch: arm`) |
| `goamd64` | filter: only `goreleaser.goamd64` (default `v1`) is imported for amd64 |
| `goarm64`, `go386`, `gomips`, `goppc64`, `goriscv64` | not read; variants of one target collide and must be narrowed with `goreleaser.ids` |
| `extra.ID` | filter (`goreleaser.ids`); the manifest records the **Hephaestus build id** |
| `extra.Binaries` + `extra.WrappedIn` | each declared path must be a file in the archive; recorded as executables |
| `extra.Format` | must agree with the file name |
| `extra.Checksum` | re-computed from the file; a mismatch is an error |

Size and SHA-256 are always computed from disk.

## Template variables

| GoReleaser | Hephaestus |
| --- | --- |
| `.ProjectName` | Supported (alias of `.Project`) |
| `.Version` | Supported (tag without `v`) |
| `.Tag` | Supported |
| `.RawVersion` | Supported: the tag exactly as given. Do not assume GoReleaser's definition. |
| `.Major`, `.Minor`, `.Patch`, `.Prerelease` | Supported (empty for non-semver tags) |
| `.FullCommit`, `.Commit` | Supported (`--commit` or `$GITHUB_SHA`) |
| `.ShortCommit` | Supported (first 7 characters) |
| `.Env.NAME` | Supported; missing keys are errors, as in GoReleaser; credential-like names are hidden unless declared in top-level `env` |
| `.Os`, `.Arch`, `.Arm` | Supported in per-target values (`.OS` is the Hephaestus spelling) |
| `.ArtifactName`, `.ArtifactPath`, `.ArtifactExt` | Supported in `brews[].url_template`, `install`, `extra_install` |
| `.PreviousTag`, `.Branch`, `.CommitDate`, `.CommitTimestamp`, `.GitURL`, `.GitTreeState`, `.IsGitClean`, `.IsGitDirty`, `.ReleaseNotes`, `.IsDraft`, `.IsSnapshot`, `.IsNightly`, `.IsSingleTarget`, `.Date`, `.Now`, `.Timestamp`, `.ModulePath`, `.ReleaseURL`, `.Summary`, `.TagSubject`, `.TagContents`, `.TagBody`, `.Runtime.*` | Not supported (validation error) |
| `.Mips`, `.Amd64`, `.Arm64`, `.Ppc64`, `.Riscv64`, `.I386`, `.Abi`, `.Target`, `.Binary` | Not supported |
| Pro variables (`.PrefixedTag`, `.Var.*`, `.Artifacts`, `.Metadata`, ...) | Not supported |

Hephaestus additions: `.Project`, `.OS`, `.Ext`.

| GoReleaser function | Hephaestus |
| --- | --- |
| `tolower`, `toupper`, `trim`, `trimprefix`, `trimsuffix`, `replace`, `urlPathEscape` | Supported |
| `title` | Supported, differs: upper-cases the first byte only |
| `split`, `contains`, `mdv2escape`, `dir`, `base`, `abs`, `join`, `filter`, `reverseFilter`, `envOrDefault`, `isEnvSet`, `map`, `indexOrDefault`, `list`, `englishJoin`, `time`, `incpatch`, `incminor`, `incmajor`, hash functions, `readFile`, `mustReadFile`, `targetVariant` | Not supported (validation error) |
| Pro functions (`in`, `reReplaceAll`, `listExclude`) | Not supported |

Hephaestus additions: `lower`, `upper` (aliases). The adapter reports a
template that uses anything unsupported with the list of available functions
(`TestLoadBrewsTemplates`).

See [migration/from-goreleaser.md](../migration/from-goreleaser.md) for a
step-by-step migration.
