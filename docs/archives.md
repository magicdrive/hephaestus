# Archives

The archives stage turns build outputs into the files you distribute. It is a
declarative packaging interface: you state exactly which files go into each
archive and where they land, and Hephaestus either produces exactly that —
byte-for-byte reproducibly — or stops with an error that names the problem.

```yaml
archives:
  - id: default
    name: "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}"
    format: tar.gz
    format_overrides:
      - os: windows
        format: zip
    files:
      - src: "dist/{{ .OS }}/{{ .Arch }}/mycli{{ .Ext }}"
        dst: "bin/mycli{{ .Ext }}"
        mode: "0755"
      - README.md
      - LICENSE
      - src: "docs/**/*.md"
        dst: docs/
        exclude:
          - docs/internal/**
      - src: docs/man/mycli.1
        dst: man/mycli.1
        required: false
        when:
          os: [darwin, linux]
```

## Archive fields

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `id` | string | `default` when there is one archive | Unique id, referenced by `brews[].ids` and `--ids`. |
| `builds` | list of build ids | all builds | Which build binaries define the targets (and the default contents). |
| `targets` | list of `{os, arch, arm}` | from the manifest | Explicit targets. Use when packaging without a build stage. |
| `name` | template | `{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}` | File name without extension. The result may contain only `A-Z a-z 0-9 . _ -`, because GitHub silently renames assets with other characters. |
| `format` | `tar.gz` \| `zip` | `tar.gz` | Archive format. |
| `format_overrides` | list of `{os, format}` | none | Per-OS format, e.g. zip on Windows. |
| `wrap_in_directory` | bool or template | `false` | Put every entry under one top-level directory (`true` uses the archive name). |
| `files` | list | the target's binaries | **The complete content list.** See below. |
| `mtime` | template: Unix seconds or RFC 3339 | `$SOURCE_DATE_EPOCH`, else `1980-01-01T00:00:00Z` | Timestamp of every entry. |
| `symlinks` | `follow` \| `reject` | `follow` | How symbolic links in sources are treated. |

Targets: an archive definition is instantiated once per target. Targets come
from `targets` if given, otherwise from the binaries recorded in the artifact
manifest (optionally restricted by `builds`). With neither, the stage fails
before writing anything.

Default contents: when `files` is omitted, each archive contains that target's
binaries at its root with mode `0755`. When `files` is present it is the whole
list — nothing is added implicitly (no automatic README/LICENSE, unlike
GoReleaser). An explicit empty list is an error.

## File entries

A file entry is either a string (shorthand for `{src: <string>}`) or:

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `src` | template | required | Project-relative file path or glob. |
| `dst` | template | `""` | Placement in the archive (see path semantics). |
| `exclude` | list of globs (templates) | none | Project-relative patterns removed from the matches. Wildcards here also match dot files. |
| `mode` | octal string | from source | Entry permission, e.g. `"0755"`. |
| `required` | bool | `true` | A literal `src` must exist; a glob must match at least one file. With `false`, nothing found is fine (reported as skipped). |
| `strip_parent` | bool | `false` | Place glob matches by base name only: `dst/<base name>`. |
| `when` | `{os: [...], arch: [...]}` | all targets | Only include for matching targets. A non-matching entry is not evaluated at all, so its `src` need not exist. |
| `info` | `{owner, group, mode, mtime}` | — | GoReleaser-compatible per-file metadata. `info.mode` is an alias of `mode`. |
| `allow_sensitive` | bool | `false` | Permit a file that looks like a credential (see Security). |

## Path semantics

These rules are exact; Hephaestus never guesses.

**Base directory.** `src` is resolved against the project root: the directory
containing the configuration file. Reading is confined to that directory with
the operating system's `openat`-style root (Go `os.Root`), so no path,
symbolic link or race can read outside it.

**Absolute paths and `..`.** `src`, `dst`, `exclude` and the rendered `name`
must be relative. Absolute paths (`/x`, `C:/x`, `\\host`), backslashes, `.`
and `..` segments, empty segments, NUL and control characters are rejected.
Templated values are checked again after rendering, so `{{ .Env.X }}` cannot
smuggle `../` in.

**Literal `src`** (no `* ? [ {`):

| `dst` | Archive path |
| --- | --- |
| omitted / `""` | the `src` path itself (`docs/a.md` → `docs/a.md`) |
| ends in `/` | `dst` + base name (`LICENSE`, `legal/` → `legal/LICENSE`) |
| anything else | exactly `dst` — a rename (`build/out/mycli`, `bin/mycli` → `bin/mycli`) |

A literal `src` that is a directory is an error; use `dir/**`.

**Glob `src`.** Syntax: `*` and `?` within one path segment, `[abc]` classes
(`[!abc]` negates), `**` for zero or more whole segments, `{a,b}` alternatives.
The glob's *base* is its leading run of literal directories: `docs` for
`docs/**/*.md`, `dist` for `dist/*/mycli`, empty for `*.md` or `{a,b}/x`.

| `dst` | Archive path of match `M` |
| --- | --- |
| omitted / `""` | `M` (project-relative path preserved) |
| set (trailing `/` optional; a glob's `dst` is always a directory) | `dst` + (`M` relative to the base) |
| set, with `strip_parent: true` | `dst` + base name of `M` |

So `docs/**/*.md` with `dst: docs/` maps `docs/guide/intro.md` to
`docs/guide/intro.md`, and with `dst: share/doc/` to
`share/doc/guide/intro.md`. Because the base comes from the pattern, the
placement of a file never depends on which *other* files happen to match.
(GoReleaser instead strips the longest common prefix of the matches, which
moves files when the match set changes.)

Glob matching never:

- matches dot files or enters dot directories through a wildcard
  (`*`, `?`, `**`). Name them explicitly (`.github/**`, `.*rc`);
- enters directories reached through symbolic links;
- matches files inside the `dist` directory unless the glob's base is itself
  inside `dist` (so earlier outputs are never re-packaged).

**Collisions.** In one archive, it is an error when two entries resolve to the
same path, when two paths differ only in letter case (they would overwrite
each other on macOS and Windows), or when a path is a file for one entry and a
directory for another. The error names both sources and their entries.

**Symbolic links.** With `symlinks: follow` (default), a link is stored as a
regular file containing the target's bytes, and only if the target resolves
inside the project root; a link that escapes the root, a broken link, and a
link to a directory are errors. With `symlinks: reject`, any link is an error.
Archives never contain link entries, so extraction cannot create links that
point outside the extraction directory.

**Other file types.** Devices, sockets and FIFOs are rejected.

**Directories.** Every parent directory of a file is emitted as an explicit
directory entry (mode `0755`). Directories are never listed directly.

**Wrapping.** With `wrap_in_directory`, every path is prefixed with the wrap
directory after the rules above (including collision checks).

## Permissions and ownership

- Default file mode: `0755` if the source is executable by its owner, else
  `0644`. Other source bits (group/other, umask effects) are ignored, so the
  result is the same on every machine.
- An explicit `mode` must be an octal string, readable by the owner, and must
  not set setuid/setgid/sticky bits or group/world write. `0444`, `0555`,
  `0600`, `0644`, `0700` and `0755` are fine; `4755`, `0777` and `0664` are
  rejected.
- Ownership is uid/gid 0 with empty user and group names, unless `info.owner`
  or `info.group` set names.

## Deterministic output

The same inputs and configuration produce the same bytes, and therefore the
same SHA-256:

| Aspect | Control |
| --- | --- |
| Entry order | Sorted by archive path (byte order), independent of file system order and declaration order. |
| Timestamps | One fixed time per archive: `mtime`, else `$SOURCE_DATE_EPOCH`, else 1980-01-01 UTC. Source file times are ignored. |
| Permissions | Normalized as above. |
| Ownership | uid/gid 0, no names. |
| tar header | USTAR when the entry fits, PAX otherwise — decided by the entry alone. No atime/ctime, no extended attributes. |
| gzip header | No file name, zero mtime, OS byte "unknown". |
| zip header | Fixed method (Deflate for files, Store for directories), Unix mode in external attributes, UTC modification time. |
| Compression | Fixed level (best compression). |

Reproducibility holds for a given Hephaestus build. Hephaestus release
binaries are built with a pinned Go toolchain; a different Go version could in
principle compress differently (archive *contents* would be identical).

Every source is hashed while planning and re-verified while writing. A file
that changes in between aborts the stage rather than producing an archive that
differs from its plan. Archives are written to a temporary file and renamed
into place, so a failure never leaves a partial archive.

## Security

| Threat | Defense |
| --- | --- |
| Path traversal (`../`, absolute paths) in `src`, `dst`, rendered templates | Lexical validation plus `os.Root` confinement. |
| Symlink escape | Links must resolve inside the root (`os.Root` enforces it); linked directories are never walked; archives contain no links. |
| Archive entry injection / Zip Slip for users | Every entry name is validated: relative, no `..`, no backslash, no control characters, no Windows-reserved characters, no trailing dot or space, at most 1024 bytes. |
| Unsafe permissions | Policy above; no setuid/setgid/sticky, no group/world write. |
| Name collisions, including case-only | Error. |
| Accidental secrets | Wildcards skip dot files. Files named like credentials (`.env*`, `*.pem`, `*.key`, `*.p12`, `id_rsa`, `.netrc`, `.npmrc`, `*.tfstate`, ...) or inside `.git`, `.ssh`, `.aws`, `.gnupg`, `.docker`, `.kube`, `.config`, and files containing a PEM private key block, are refused unless the entry sets `allow_sensitive: true`. |
| Huge files / too many files | `limits.max_file_size` (default 2 GiB), `limits.max_total_size` (default 4 GiB per archive), `limits.max_files` (default 10000 entries, also bounding glob expansion). |
| Unexpected glob matches | Globs are bounded (`**` at most 4 times, at most 64 brace alternatives), skip dot files and `dist`, and every included file is listed in the manifest. |

## Manifest record

Each archive is recorded in the artifact manifest (`dist/hephaestus-manifest.json`):

```json
{
  "kind": "archive",
  "id": "default",
  "name": "mycli_1.2.3_darwin_arm64.tar.gz",
  "path": "dist/mycli_1.2.3_darwin_arm64.tar.gz",
  "os": "darwin",
  "arch": "arm64",
  "format": "tar.gz",
  "sha256": "…",
  "size": 1234567,
  "executables": ["bin/mycli"],
  "files": [
    { "path": "bin", "type": "dir", "mode": "0755" },
    { "path": "bin/mycli", "type": "file", "mode": "0755", "size": 4096000, "sha256": "…", "source": "dist/darwin/arm64/mycli" }
  ],
  "source": { "stage": "archive" }
}
```

The GitHub Release publisher uploads exactly these files and verifies them
against `sha256`. The Homebrew formula generator takes URLs and checksums from
here, and checks that every path the formula's `install` block references
(for example `bin.install "bin/mycli"`) exists in every archive.
