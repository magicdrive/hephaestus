# Threat model

Hephaestus sits at the end of a software supply chain: what it publishes is
downloaded and executed by users, and a Homebrew formula is Ruby code that
runs on their machines. This document lists the threats Hephaestus is
designed against, where each defense lives, and what remains. The policy
summary is [SECURITY.md](../../SECURITY.md).

## Assets

| Asset | Why it matters |
| --- | --- |
| Release token (`GITHUB_TOKEN`, `contents: write`) | Can create and change releases and assets of the repository. |
| Tap token (`HOMEBREW_TAP_GITHUB_TOKEN` or `repository.token`) | Can change formulae in the tap: code execution on every user who installs or upgrades. |
| Published archives and checksum file | What users download and run. |
| Generated formula | Ruby code executed by `brew`. |
| Artifact Manifest | Carries digests, URLs and archive contents between stages. |
| Files outside the project root, and secrets inside it | Must not be read, packaged or published. |

## Attackers and trust levels

| Actor | Trust | Can |
| --- | --- | --- |
| Maintainer writing `hephaestus.yaml` | Trusted (the configuration is code) | Anything the configuration allows |
| Pull request author (fork or branch) | Untrusted until merged | Change the configuration, build scripts and files if a workflow runs their code |
| Third-party code run by the build (dependencies, compilers, generators) | Untrusted | Run with the build command's environment and file access |
| Producer of imported artifacts (another job, GoReleaser, a download) | Semi-trusted | Supply archives and `artifacts.json` |
| Network attacker / misconfigured endpoint | Untrusted | Observe or redirect HTTP traffic |
| Concurrent writer to the tap or release | Semi-trusted | Change state between Hephaestus's reads and writes |

GitHub's API at the configured URL is trusted.

## Threats

Each entry: **asset · attacker**, defenses with their location, residual risk.

### Supply chain attack

**Assets:** published archives, tap. **Attacker:** compromised dependency or
build tool; compromised Hephaestus distribution.

- The release and tap tokens, the variables `brews[].repository.token` reads,
  the runner's `ACTIONS_*` tokens and credential-looking variables are
  removed from build commands' environment unless a build names them in its
  `env` (`internal/build`, `cmd/hephaestus`). On Linux Hephaestus marks
  itself non-dumpable, so same-user processes cannot read its
  `/proc/PID/environ`. The action runs local stages (`build`, `archive`,
  `checksum`, and the local phase of `run`) in a step whose environment holds
  no token, and the publish phase in a later step (`action.yml`).
- The action's self-build uses a new, empty `GOCACHE` and `GOMODCACHE`, so a
  cache restored by an earlier step cannot alter Hephaestus.
- The action builds Hephaestus from its own pinned source with a fixed Go
  environment; no binary is downloaded (`action.yml`). Hephaestus's own
  releases are gated on the full CI, verified by `scripts/verify-dist.sh`,
  and carry build provenance attestations (`.github/workflows/release.yml`).
  That workflow builds in a job with `contents: read` only and publishes
  from a separate job that runs no project build command, so build code
  never holds the release token, the tap token or an OIDC token (which
  could sign provenance for other files); `repo_test.go` enforces it.
- One direct dependency (`gopkg.in/yaml.v3`); CI runs govulncheck.
- Recommended: build and publish in separate jobs
  ([github-action.md](../github-action.md)).

**Residual:** a compromised build can still produce malicious binaries; that
is outside what a packaging tool can detect. Removing tokens from the build
environment is not isolation: build code runs as the same user, can read the
environment of Hephaestus's ancestors (the step's shell; on macOS also
Hephaestus itself, with `ps -E`), and a process that escapes the step can
wait for a later step of the same job and read its environment. A build
running in the same job as publishing therefore shares its credentials;
only separate jobs isolate them. When one process runs both build and
publishing stages with a token set, Hephaestus warns.

### Artifact tampering

**Assets:** archives, checksum file, formula, manifest. **Attacker:** a
process that modifies files between stages or jobs.

- Every file is hashed when recorded; a source that changes between planning
  and writing aborts the archive stage (`internal/archive`); the checksum
  stage re-hashes archives against the manifest (`internal/checksum`); the
  release stage re-hashes every asset before upload, hashes while uploading
  and compares with GitHub's reported size and digest (`internal/release`);
  the tap stage refuses a formula that differs from its manifest digest
  (`internal/tap`).
- GoReleaser's `extra.Checksum` is verified, never copied
  (`internal/build/goreleaser.go`).
- The manifest is strictly parsed (unknown fields rejected) and written
  atomically (`internal/manifest`).

**Residual:** an attacker who can rewrite both the files and the manifest
(for example a compromised build job whose `dist/` is passed to the publish
job) produces a consistent forgery. Provenance attestation and reproducible
builds are the way to detect that, outside Hephaestus.

### Path traversal

**Assets:** files outside the project root. **Attacker:** configuration or
template values, imported archive or `artifacts.json` paths.

- Every configured, rendered or imported path passes
  `pathutil.ValidateRelative`: no absolute paths, drive letters, `..`, `.`,
  empty segments, backslashes, control or Unicode format characters, Windows
  reserved names, overlong names.
- Reads and writes under the project root (sources, build outputs, `dist/`,
  manifest, checksum file, formula, `notes_file`) go through `os.Root`.
- Templated values are validated again after rendering.

**Residual:** none known for paths Hephaestus opens. Build commands are
arbitrary programs and can access whatever their user can.

### Zip Slip

**Assets:** the machines of users who extract the archives; Hephaestus's own
inspection. **Attacker:** a malicious imported archive.

- Archives Hephaestus writes contain only validated relative entry names and
  no link entries (`internal/archive/write.go`).
- Imported archives are inspected, never extracted (`archive.Inspect`): names
  that are absolute, contain `..` or are otherwise non-portable, links,
  devices, FIFOs, sparse files, setuid/setgid/sticky bits, duplicate or
  case-colliding paths, entries after the tar end marker, zip entries whose
  local header name differs from the central directory name, and
  decompression bombs are rejected.

**Residual:** extractors with unusual parsing rules not covered by these
checks. Unicode normalization (NFC/NFD) collisions are not detected.

### Symlink attack

**Assets:** files outside the root; secrets inside it. **Attacker:** a
symbolic link committed to the repository.

- `symlinks: follow` (default) stores a link's target content only when it
  resolves inside the root (`os.Root` enforces it); links to directories are
  never walked; `symlinks: reject` refuses every link, including links in
  intermediate directories.
- Sensitive-file checks apply to a link's resolved target as well as to its
  name.
- Build outputs and prebuilt files must be regular files inside the root
  (`build.openVerified`).
- `dist/` and other outputs are written through `os.Root`, so a symlinked
  `dist` cannot redirect writes outside the root.

**Residual:** a link to another file inside the root is followed by default;
use `symlinks: reject` to forbid links entirely.

### Accidental secret disclosure (archives)

**Assets:** credentials in the working tree (`.env`, keys, `.git/config`
with an `extraheader` token). **Attacker:** configuration mistakes, malicious
pull requests.

- Wildcards never match dot files; globs never re-package `dist/`.
- Files named like credentials (checked case-insensitively) or inside
  `.git`, `.ssh`, `.aws`, `.gnupg`, `.docker`, `.kube`, `.config`, and files
  containing a private-key block, are refused unless the entry sets
  `allow_sensitive: true` (`internal/archive/sensitive.go`). Imported
  archives get the same name check.
- Every included file is listed in the manifest.

**Residual:** detection is best effort (names and key markers); a secret in
an ordinary-looking file is not detected.

### Ruby injection

**Assets:** users who install the formula. **Attacker:** configuration
values, template values (tag, environment), manifest data.

- Every data value (`desc`, `homepage`, `version`, URLs, `sha256`, headers,
  dependency and conflict names, caveats) is an escaped Ruby literal:
  backslashes, quotes, `#{`, `#@`, `#$`, control and invisible characters
  (including bidirectional overrides) are escaped; invalid UTF-8 is an error
  (`homebrew.RubyString`, `rubyHeredoc`).
- `license` is parsed as SPDX; `download_strategy` and `custom_require` are
  restricted by pattern; the formula name and class name are validated.
- Code fields (`install`, `test`, `service`, `post_install`, `custom_block`)
  are trusted configuration but cannot escape their block (`__END__`,
  `=begin`, plain heredocs, unterminated heredocs, control characters are
  rejected), and template values substituted into them must use a safe
  alphabet (`homebrew.renderCode`).

**Residual:** code fields are executable by design; a malicious maintainer or
merged pull request can write arbitrary Ruby.

### Shell injection

**Assets:** the runner. **Attacker:** action inputs, template values.

- `action.yml` passes every input through environment variables, never
  `${{ }}` interpolation into scripts; `command` is checked against a fixed
  list; arguments are built as a bash array.
- A list-form `command` is executed without a shell.
- Tags are restricted to `[A-Za-z0-9._+-]`, so `.Tag` and `.Version` are
  safe in shell commands.

**Residual:** a string-form `command` is a shell script; embedding
`{{ .Env.X }}` of an attacker-controlled variable (for example a branch name)
into it is injection. Prefer the list form or pass values through the
environment.

### Argument injection

**Assets:** the runner, build tools. **Attacker:** template values that start
with `-`.

- List-form commands pass each element as one argument; program names
  without a separator are resolved only through absolute `PATH` entries
  (`build.resolveProgram`).
- Tags may not start with `-`; branch names are validated with
  `git check-ref-format` rules.

**Residual:** a template value placed where a tool parses options can still
be read as an option by that tool.

### Credential leak

**Assets:** release and tap tokens. **Attacker:** logs, summaries, published
text, build code, configuration.

- Tokens are read from the environment, masked with `::add-mask::`, and
  replaced by `***` in errors, the step summary and outputs.
- `.Env` hides the token variables, the variables `repository.token` reads
  and every variable whose name contains `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `PASSPHRASE`,
  `CREDENTIAL`, `PRIVATE_KEY`, `API_KEY`, `APIKEY` or `ACCESS_KEY`, or has a
  `_`-separated segment `PAT`, `AUTH`, `KEY`, `PASS` or `PWD` (ignoring case),
  unless declared in top-level `env` (`internal/tmpl`). Only `builds[].env`
  values (the explicit opt-in for passing a credential to a build) and
  `brews[].repository.token` are rendered against the complete environment,
  through a function templates cannot call.
- Every token value (and each rendered `repository.token`) is registered:
  a rendered template value that contains one is an error, so a formula,
  release body, commit message or pull request title cannot carry it.
- Redaction also covers base64 (including `x-access-token:<token>`),
  URL-encoded and quoted forms. Known tokens are redacted whatever their
  length; values of variables recognized only by name, from 8 characters.
- The GitHub client never puts the token in an error message.
- Build commands do not receive the tokens in their environment (see
  supply chain attack for what that does not prevent).
- Tokens are sent only over https (http only to loopback).
- `preflight` and `status` are read-only: their GitHub access is an
  interface of GET methods only (tests fail on any other request), and
  their annotations, step summary and `--report` file go through the same
  redaction. The action's preflight step receives the release token only
  and an empty tap token.

**Residual:** a credential under a name the heuristic misses is visible to
`.Env`; transformed values other than the encodings above (split, hashed,
re-encoded) are not recognized. A maintainer can deliberately declare a
secret in top-level `env` and print it.

### Malicious redirect

**Assets:** tokens. **Attacker:** a redirecting server or object storage.

- The token is sent only to the origin of the request; a redirect to another
  origin is followed without `Authorization`; https-to-http redirects are
  refused; pagination links are followed only within the API origin; uploads
  always go to the configured upload URL, never to a URL from a response
  (`internal/github`).
- API and upload URLs must be https (http only for loopback); a GitHub
  Enterprise Server upload URL is derived from its API URL, so a GHES token
  is never sent to github.com.

**Residual:** a configuration that points `api_url` at a hostile https server
sends the tokens there. Review `release.github.api_url` / `upload_url`
changes.

### Unsafe Git operations

**Assets:** the tap's history. **Attacker:** concurrent writers, stale runs.

- References are only created, never updated or force-pushed; commits go
  through the contents API with the blob SHA as an optimistic lock; conflicts
  are re-read and retried a bounded number of times (`internal/tap`).
- An existing head branch whose changes are not only the formula is an
  error.
- A newer formula on the tap is not replaced by an older version unless
  downgrades are explicitly allowed.
- Identical content produces no commit.

**Residual:** a pull request still needs human review before merge; direct
mode writes to the branch immediately.

### Release asset collision

**Assets:** release integrity. **Attacker:** name clashes, stale assets.

- Asset names must match `^[A-Za-z0-9_][A-Za-z0-9._-]*$` (GitHub renames
  others) and be unique case-insensitively; the manifest forbids two
  artifacts of one kind with the same name.
- A same-named asset with different content is an error unless
  `replace_existing_assets: true` and the release is a draft; a published
  release's assets are never replaced or deleted. Identical assets are left
  alone; an incomplete upload on a draft older than the grace period is
  deleted and retried. Unexpected assets on a draft are an error.

### Concurrent publication

**Assets:** release and tap state. **Attacker:** two runs for one tag, lost
responses.

- POST requests are never retried blindly; a lost create, upload, ref or pull
  request is followed by a lookup (`internal/release`, `internal/tap`).
- A run whose new draft loses to another release for the tag (a published
  one, or a lower-id draft) stops with a conflict; no release is ever
  deleted, so another run's draft cannot be destroyed.
- The draft is re-read right before publishing: a release published
  meanwhile is not edited, and a draft whose assets changed is not
  published.
- Pull requests for the same head are reused; a ref created concurrently is
  accepted.
- Recommended: `concurrency: release-${{ github.ref }}` with
  `cancel-in-progress: false`.

**Residual:** without the concurrency group, two runs may still race on
asset uploads; each run verifies what it finds.

### Workflow hardening (Hephaestus's own workflows)

- Every action is pinned to a full commit SHA; checkouts use
  `persist-credentials: false`.
- Workflows default to `permissions: contents: read`; write permissions
  (`contents`, `id-token`, `attestations`) are granted per job, only in the
  release job, which runs in the `release` environment.
- No workflow uses `pull_request_target` or `workflow_run`; no `${{ }}`
  expression is interpolated into a `run:` script (values pass through
  `env`).
- The tap token is a secret of the `release` environment, passed only to
  the tap rehearsal and publication steps, which run only when the
  release plan publishes the tap (stable releases).

### Untrusted pull request

**Assets:** all tokens. **Attacker:** a fork author.

- Documentation forbids publishing stages on `pull_request` /
  `pull_request_target` and checking out fork code with secrets
  ([github-action.md](../github-action.md#untrusted-pull-requests)).
- `--snapshot` refuses the release and tap stages; the action passes tokens
  only to publishing commands; `github-token: ""` disables it entirely.

**Residual:** Hephaestus cannot see how a workflow was triggered. A workflow
that runs `pull_request_target` with a checkout of the fork gives that code
everything the job has.

## Out of scope

- Signing (GPG, cosign), SBOMs and vulnerability scanning of the packaged
  software.
- The correctness of the build itself.
- Compromise of GitHub, the runner image, or the maintainer's account.
