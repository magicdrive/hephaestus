// Command publish releases Hephaestus itself: it checks that a version can
// be released, creates and pushes the annotated release tag, then follows
// the release workflow that the tag push starts and verifies the result.
// The release workflow (.github/workflows/release.yml) remains the only
// publisher. Run it through the Makefile:
//
//	make publish v1.2.3          # or VERSION=v1.2.3
//	make publish-check v1.2.3    # preflight only
//	make publish-status v1.2.3   # what exists for a version
//	make publish-watch v1.2.3    # follow an already pushed tag
//	make publish-help
//
// It is `hephaestus publish` (internal/publish/cli) with this repository's
// own usage and help text, built from the source being released rather than
// from an installed hephaestus. It lives under internal/ because it is this
// repository's release tool; other projects run `hephaestus publish`
// (docs/publish.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/magicdrive/hephaestus/internal/publish"
	"github.com/magicdrive/hephaestus/internal/publish/cli"
)

// Environment variables the Makefile passes the version through: values
// never pass through a shell command line.
const (
	envArgs    = cli.EnvArgs
	envVersion = cli.EnvVersion
	envTag     = cli.EnvTag
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage: publish <command> [flags] <version>

Commands (normally run through make):
  publish   preflight, confirm, create and push the annotated tag, follow the
            release workflow run and verify the release
  check     preflight only: no tag, no push, no GitHub writes
  status    show the tag, release run, release, assets, attestations and tap
            state of a version (read-only)
  watch     follow the release run of an already pushed tag and verify it
  plan      print what the release workflow does for a tag (key=value lines
            for $GITHUB_OUTPUT): prerelease and whether the tap is published
  help      show the safety notes

Versions are release tags: vMAJOR.MINOR.PATCH[-PRERELEASE] (SemVer, a leading
"v", no build metadata), e.g. v1.2.3 or v0.1.0-rc.1.

Flags:
`

const help = `make publish vX.Y.Z (or VERSION=vX.Y.Z) releases Hephaestus:

  1. preflight (read-only, plus a fetch of origin/main): on main, clean worktree,
     HEAD = origin/main, origin is the release repository, the tag exists neither
     locally nor on origin, CI (ci.yml push run) succeeded on exactly this commit,
     the v* tag ruleset is active, the main ruleset requires ci-gate, environment
     "release" deploys only from v* tags, no release or draft exists yet; for a
     stable version also the tap: token secret present (name only), pull request
     mode, formula directory, no stale branch or pull request
  2. confirmation (default No; YES=1 for an explicit non-interactive run)
  3. annotated tag at the checked commit, re-check, then a plain (never forced)
     push of that one tag. The push is the only release trigger and cannot be
     taken back: the tag is never moved or deleted afterwards.
  4. the release.yml run for this tag and commit is found and followed
  5. verification: release published, prerelease flag, "latest", 7 assets,
     SHA-256 against the checksum file, provenance attestation of every asset
     (signer workflow, tag ref, commit, GitHub-hosted runner), and the tap:
     untouched for a prerelease, a formula pull request for a stable version
     (never merged by publish)

Prereleases (v1.2.3-rc.1) are published as GitHub prereleases, never "latest",
and never update the Homebrew tap (skip_upload: auto); the tap token is not
even passed to the workflow step.

If monitoring stops (network, Ctrl-C, timeout) the release keeps running on
GitHub: make publish-watch vX.Y.Z resumes, make publish-status vX.Y.Z shows
where it stands. Details: docs/releasing.md.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return cli.Run(ctx, args, stdout, stderr, cli.Config{Name: "publish", Usage: usage, Help: help, Gate: actionVersionMatches})
}

// actionVersionFile names the release whose binary the GitHub Action
// downloads, relative to the repository root. Hephaestus must not release
// another tag: the release workflow enforces it, and publish and check stop
// early, before anything is tagged.
const actionVersionFile = "action/VERSION"

// actionVersionMatches requires action/VERSION, when the repository root
// (the directory of the configuration) has one, to name tag v.
func actionVersionMatches(configPath string, v publish.Version) error {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), filepath.FromSlash(actionVersionFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if got := strings.NewReplacer("\r", "", "\n", "").Replace(string(b)); got != v.Tag {
		return fmt.Errorf("%s is %s, not %s: set it to %s in a pull request first", actionVersionFile, got, v.Tag, v.Tag)
	}
	return nil
}
