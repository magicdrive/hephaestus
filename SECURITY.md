# Security Policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Please do not describe a vulnerability in a public issue. Report it privately
through
[GitHub Security Advisories](https://github.com/magicdrive/hephaestus/security/advisories/new)
with the Hephaestus version (`hephaestus version`, or the action's commit),
your OS or runner, the configuration (without secrets) and the steps to
reproduce. If that form is not available, open an issue that only asks for a
private contact, without any details. You should get a first reply within a
week.

## Scope

Hephaestus builds, packages and publishes release artifacts. It runs with the
permissions of the user or workflow that starts it, and it handles two kinds
of credentials: a token that can write releases to your repository and a
token that can write to your Homebrew tap. A vulnerability is anything that
lets an input Hephaestus does not fully trust change what is published, read
or write outside the project root, or obtain a token. The detailed analysis
is [docs/security/threat-model.md](docs/security/threat-model.md).

## Trust boundaries

What Hephaestus trusts:

- **The configuration is code.** `builds[].command` runs arbitrary commands,
  and `install`, `test`, `service`, `post_install` and `custom_block` become
  Ruby code that users of your tap execute. Treat `hephaestus.yaml` like a
  workflow file: review every change, protect it with `CODEOWNERS`, and never
  run Hephaestus on configurations you do not trust. `hephaestus check`
  validates a file without running anything.
- **GitHub's API** at the configured API URL.

What Hephaestus does not trust, and how it defends itself:

- **Build commands and the code they run** (compilers, dependencies, code
  generators). The release and tap tokens, the variables read by
  `brews[].repository.token`, the runner's `ACTIONS_*` tokens and every
  variable whose name looks like a credential are removed from the
  environment build commands receive, unless a build names them in its
  `env`. This is defense in depth, not isolation: build code runs as the
  same user and may read the environment of Hephaestus or its parent
  processes (`ps -E` on macOS; `/proc/PID/environ` on Linux, where
  Hephaestus makes itself non-dumpable but its parent shell is not). The
  GitHub Action runs local stages in a step without tokens; for publishing,
  build and publish in separate jobs. Build outputs are verified inside the
  project root and hashed from the bytes on disk.
- **Project files selected by archives.** Reads go through `os.Root`, so no
  path, symbolic link or race reads outside the project root. Archives never
  contain link entries; unsafe modes, path traversal, case-only collisions
  and files that look like credentials (checked case-insensitively, also at a
  symbolic link's target) are refused unless explicitly allowed.
- **Imported archives and GoReleaser output.** Archives are inspected
  without extraction and rejected for traversal, links, special files, unsafe
  modes, collisions, decompression bombs, data after the tar end marker, and
  zip entries whose local and central names differ. Credential-looking member
  names are refused unless allowed. A checksum claimed by another tool is
  verified, never copied.
- **Template values.** Values substituted into Ruby code must come from a
  safe alphabet; data values are escaped Ruby literals. `.Env` does not
  expose variables whose name looks like a credential, or that
  `repository.token` reads, unless they are declared in the configuration's
  top-level `env`, and a rendered value that contains a known token is an
  error. Name detection is a heuristic: a credential under an unusual name
  is visible.
- **Network responses.** Tokens are sent only to their own origin over https
  (plain http only to a loopback address); redirects to other hosts drop the
  token; https-to-http redirects are refused; response sizes are bounded.

## Publication guarantees

- A release is created as a draft and published only after every asset has
  been re-hashed against the manifest, uploaded, and verified against what
  GitHub reports. Unexpected assets on a draft are an error. Concurrent runs
  for one tag converge on one draft.
- A published immutable release is never modified.
- Tap updates never force-push. An existing pull request branch that contains
  changes other than the formula is an error. A newer formula on the tap is
  not overwritten by an older version unless downgrades are explicitly
  allowed.
- When the tag does not exist yet, it is created at the build commit
  (`--commit` or `GITHUB_SHA`) unless `target_commitish` says otherwise.
- Known token values are replaced by `***` in errors, the step summary and
  action outputs, and registered with `::add-mask::` for the log.

## GitHub Actions

- Pin `magicdrive/hephaestus` to a full commit SHA. The action builds
  Hephaestus from that commit with a fixed Go environment.
- Never run publishing stages on `pull_request` or `pull_request_target`,
  and never check out a fork's code in a workflow that has secrets.
- Build in a job without write permissions; publish from a separate job,
  triggered by tags, in a protected environment, with
  `concurrency: release-${{ github.ref }}`.
- Use a GitHub App token or a fine-grained token limited to the tap for
  `tap-token`.

Details: [docs/github-action.md](docs/github-action.md).

## Determinism

The same inputs, configuration and Hephaestus build produce byte-identical
archives and manifests: entries are sorted, timestamps fixed
(`SOURCE_DATE_EPOCH` or 1980-01-01), modes and ownership normalized, and
compression headers fixed. Reproducibility lets anyone rebuild a release and
compare checksums. Hephaestus's own release archives carry build provenance
attestations (`gh attestation verify`).

## Known limitations

- Credential detection in archives is a best-effort safety net based on file
  names and private-key markers, not a guarantee. Review what you package.
- Build code in the same job as publishing can obtain the tokens (from a
  parent process's environment, or by outliving its step and reading a later
  one). Only separate jobs isolate build from publish credentials.
- Unicode normalization (NFC/NFD) collisions in archive paths are not
  detected.
- A build job that is compromised can write a consistent but malicious
  `dist/` (files and manifest together); splitting build and publish jobs
  limits what it can reach, not what it can produce.
- Ruby code in the configuration is trusted by design.

Reports about these documented limits are still welcome if you find a way
around them.
