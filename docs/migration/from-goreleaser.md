# Migrating from GoReleaser

You do not have to switch everything at once. Hephaestus can take over one
piece at a time, starting with the Homebrew formula (GoReleaser has
deprecated `brews` in favor of casks), while GoReleaser keeps building and
releasing.

| Step | GoReleaser does | Hephaestus does |
| --- | --- | --- |
| 1 | build, archive, checksum, release | formula, tap |
| 2 | build | archive, checksum, release, formula, tap |
| 3 | — | everything (`command` builds) |

Field-by-field differences: [compatibility/goreleaser.md](../compatibility/goreleaser.md).

## Step 1: Hephaestus publishes the formula

1. Remove `brews` from `.goreleaser.yaml` (or keep it with
   `skip_upload: true` while you compare outputs).
2. Create `hephaestus.yaml` that imports GoReleaser's archives and holds the
   `brews` entry:

```yaml
version: 1

project:
  name: mycli
  description: Friendly tool for managing widgets
  homepage: https://github.com/acme/mycli
  license: MIT

builds:
  - id: goreleaser
    goreleaser:
      archives: true
      ids: [default]

brews:
  - name: mycli
    directory: Formula
    install: |
      bin.install "mycli"
    test: |
      assert_match version.to_s, shell_output("#{bin}/mycli --version")
    repository:
      owner: acme
      name: homebrew-tap
```

3. Run after `goreleaser release`:

```sh
hephaestus run --tag "$TAG" --stages build,formula,tap --fresh   # import dist/artifacts.json, then publish the formula
```

The import and the formula run as two invocations because, in this version,
the preflight check of one `run` does not count archives imported by its own
build stage. `goreleaser.ids` selects GoReleaser archive ids; `brews[].ids`
refers to Hephaestus `archives` ids and cannot select imported archives, so
leave it out. Download URLs default to
`https://github.com/<GITHUB_REPOSITORY>/releases/download/<tag>/<archive>`,
the release GoReleaser created; set `url_template` for another host.

A complete setup: [examples/goreleaser-formula-only](../../examples/goreleaser-formula-only/README.md)
and [examples/workflows/goreleaser-formula-only.yml](../../examples/workflows/goreleaser-formula-only.yml).

### Copying `brews`

Copy the list item by item under `brews:` in `hephaestus.yaml`, then run
`hephaestus check`. Unknown or unsupported keys are errors with a line number,
so nothing is silently dropped. Typical edits:

| In `.goreleaser.yaml` | In `hephaestus.yaml` |
| --- | --- |
| `repository.pull_request.enabled` unset (direct push) | Hephaestus defaults to a pull request; add `pull_request: {enabled: false}` to keep direct pushes |
| `commit_author: {name: goreleaserbot, ...}` unset | No default author: commits belong to the token's identity; set `commit_author` if you want one |
| `ids` naming GoReleaser archive ids | Move the selection to `builds[].goreleaser.ids` (step 1) or to Hephaestus `archives` ids (steps 2–3) |
| `goarm` / `goamd64` | Ignored with a warning; select the amd64 level with `builds[].goreleaser.goamd64` |
| `install` using `{{ .Os }}` / `{{ .Arch }}` | One `def install` for every platform: branch in Ruby (`OS.mac?`, `Hardware::CPU.arm?`) |
| `dependencies[].version` | Ignored with a warning; depend on a versioned formula (`name@1.2`) |
| `plist` | Use `service` |
| `tap`, `folder` | Read as `repository`, `directory`, with a warning |
| `repository.git`, `commit_author.signing`, `pull_request.body`, `pull_request.token`, `alternative_names` | Not supported; remove |
| Templates in `name`, `license`, `dependencies`, `conflicts`, `skip_upload`, `download_strategy`, `custom_require` | Not templates in Hephaestus; write literal values |
| `{{ .Date }}`, `{{ .PreviousTag }}`, `split`, `time`, ... | Not available; see the template table in the compatibility page |

A minimal entry that is valid in both tools:

```yaml
brews:
  - name: mycli
    directory: Formula
    homepage: https://github.com/acme/mycli
    description: Friendly tool for managing widgets
    license: MIT
    commit_msg_template: "Brew formula update for {{ .ProjectName }} version {{ .Tag }}"
    install: |
      bin.install "mycli"
    repository:
      owner: acme
      name: homebrew-tap
      branch: "mycli-{{ .Version }}"
      pull_request:
        enabled: true
```

Expect the generated formula to differ in layout from GoReleaser's (nested
`on_arm`/`on_intel` blocks, one `def install`, `version` omitted when
Homebrew can infer it, a default `test`); it is checked with
`brew audit --strict` in Hephaestus's CI.

## Step 2: Hephaestus packages and releases

Keep GoReleaser as the compiler only (`goreleaser build --clean`), import its
binaries, and declare archives in Hephaestus:

```yaml
version: 1

project:
  name: mycli
  description: Friendly tool for managing widgets
  homepage: https://github.com/acme/mycli
  license: MIT

builds:
  - id: goreleaser
    goreleaser:
      goamd64: v1

archives:
  - id: default
    builds: [goreleaser]
    format_overrides:
      - os: windows
        format: zip
    files:
      # GoReleaser's binary directories carry a variant suffix
      # (dist/mycli_linux_amd64_v1/); strip_parent places the match at the root.
      - src: "dist/mycli_{{ .OS }}_{{ .Arch }}*/mycli{{ .Ext }}"
        strip_parent: true
        mode: "0755"
      - LICENSE
      - README.md

checksum:
  name: "{{ .Project }}_checksums.txt"

release:
  github:
    owner: acme
    name: mycli
    generate_notes: true

brews:
  - name: mycli
    directory: Formula
    repository:
      owner: acme
      name: homebrew-tap
```

Archive `files` are project paths. The glob above must match exactly one
binary per target; if GoReleaser built several variants of one target (for
example several `goamd64` levels, or `arm` and `arm64` sharing a prefix),
make the pattern more specific. Alternatively omit `files`: each archive then
holds the target's imported binaries at its root with mode `0755`, and
nothing else.

Differences from GoReleaser's archives to review:

- `files` lists everything: LICENSE and README are **not** added
  automatically.
- `name_template` is `name`, `format_overrides[].goos` is `os`, and only
  `tar.gz` and `zip` exist.
- Glob placement is based on the pattern's literal prefix, not on the
  common prefix of the matches.

Then `goreleaser build --clean && hephaestus run --tag "$TAG" --fresh`.

## Step 3: Hephaestus builds too

Replace the import with a `command` build. For Go:

```yaml
builds:
  - id: mycli
    matrix:
      os: [darwin, linux, windows]
      arch: [amd64, arm64]
    command: [go, build, -trimpath, -ldflags, "-s -w -X main.version={{ .Version }}", -o, "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}", ./cmd/mycli]
    env:
      GOOS: "{{ .OS }}"
      GOARCH: "{{ .Arch }}"
      CGO_ENABLED: "0"
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"
```

and point archive `files` at `dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}`.
GoReleaser features without a Hephaestus equivalent (`ldflags` templates with
`.Date`, `hooks`, `universal_binaries`, `upx`, nFPM packages, Docker,
signing, SBOMs, changelog generation) must move into your build command or
other tools, or stay with GoReleaser. See the [Go example](../../examples/go/README.md).

## Checklist

- [ ] `hephaestus check` passes with no unexpected warnings.
- [ ] `hephaestus run --snapshot --stages build,archive,checksum` produces the
      archive names and contents you expect (`tar -tzvf`).
- [ ] A dry run (`--dry-run`) against the real repository shows the planned
      release and tap writes.
- [ ] The tap token is a GitHub App token or a fine-grained token limited to
      the tap, stored in a protected environment.
- [ ] The release workflow runs only on tags, with
      `concurrency: release-${{ github.ref }}`.
