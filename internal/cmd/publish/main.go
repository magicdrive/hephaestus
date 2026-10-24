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
// It lives under internal/ because it is this repository's release tool,
// not part of the hephaestus command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/preflight"
	"github.com/magicdrive/hephaestus/internal/publish"
)

// Environment variables the Makefile passes the version through: values
// never pass through a shell command line.
const (
	envArgs    = "HEPHAESTUS_PUBLISH_ARGS"
	envVersion = "HEPHAESTUS_PUBLISH_VERSION"
	envTag     = "HEPHAESTUS_PUBLISH_TAG"
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
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("publish "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	configPath := fs.String("config", "hephaestus.yaml", "configuration file")
	yes := fs.Bool("yes", false, "confirm without a prompt (explicit non-interactive mode)")
	fromMake := fs.Bool("from-make", false, "read the version from the environment the Makefile sets")
	discover := fs.Duration("discover-timeout", 5*time.Minute, "how long to wait for the release run to appear")
	watchTimeout := fs.Duration("watch-timeout", 90*time.Minute, "how long to wait for the release run to finish")
	poll := fs.Duration("poll", 15*time.Second, "polling interval")
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, help)
		return 0
	case "publish", "check", "status", "watch", "plan":
	default:
		fmt.Fprintf(stderr, "publish: unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	a := publish.Args{Positional: fs.Args()}
	if *fromMake {
		if len(fs.Args()) > 0 {
			fmt.Fprintln(stderr, "publish: -from-make takes the version from the environment only")
			return 2
		}
		a = publish.Args{Positional: strings.Fields(os.Getenv(envArgs)), VersionVar: os.Getenv(envVersion), TagVar: os.Getenv(envTag)}
	}
	raw, err := publish.ResolveArgs(a)
	if err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 2
	}
	v, err := publish.ParseVersion(raw)
	if err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 2
	}
	p, err := publish.LoadProject(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}
	pl, err := p.Plan(v)
	if err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}
	if cmd == "plan" {
		return printPlan(stdout, pl)
	}

	d := &publish.Deps{
		Git: publish.GitCLI{Run: publish.ExecRunner},
		GH:  publish.GHCLI{Run: publish.ExecRunner, Sleep: publish.Sleep},
		Out: stdout, In: os.Stdin, Interactive: isTerminal(os.Stdin),
		Now: time.Now, Sleep: publish.Sleep,
	}
	d.Generic = genericPreflight(p, *configPath)
	opts := publish.Options{Yes: *yes, DiscoverTimeout: *discover, WatchTimeout: *watchTimeout, Poll: *poll}

	switch cmd {
	case "check":
		rep := publish.Preflight(ctx, d, p, pl)
		rep.Print(stdout)
		if !rep.OK() {
			fmt.Fprintf(stdout, "\npreflight FAILED for %s\n", v.Tag)
			return 1
		}
		fmt.Fprintf(stdout, "\npreflight passed: make publish %s would tag %s\n", v.Tag, rep.Commit)
		return 0
	case "status":
		s, err := publish.Observe(ctx, d, p, pl)
		if err != nil {
			fmt.Fprintf(stderr, "publish: status: %v\n", err)
			return 1
		}
		s.Print(d, pl)
		if s.State == publish.Failed {
			return 1
		}
		return 0
	}

	var o *publish.Outcome
	if cmd == "publish" {
		o = publish.Publish(ctx, d, p, pl, opts)
	} else {
		o = publish.Watch(ctx, d, p, pl, opts)
	}
	summarize(stdout, v, o)
	return o.ExitCode()
}

func printPlan(w io.Writer, pl *publish.Plan) int {
	tap := "publish"
	if pl.TapSkipped() {
		tap = "skip"
	}
	fmt.Fprintf(w, "tag=%s\nversion=%s\nprerelease=%t\ntap=%s\n", pl.Version.Tag, pl.Version.Version, pl.Prerelease, tap)
	return 0
}

func summarize(w io.Writer, v publish.Version, o *publish.Outcome) {
	fmt.Fprintln(w)
	switch {
	case o.State == publish.Completed:
		fmt.Fprintf(w, "%s: released and verified (%s)\n", v.Tag, o.State)
	case o.MonitorLost:
		fmt.Fprintf(w, "%s: outcome unknown, last state %s: %v\n", v.Tag, o.State, o.Err)
	case errors.Is(o.Err, publish.ErrDeclined):
		fmt.Fprintf(w, "%s: %v\n", v.Tag, o.Err)
	default:
		fmt.Fprintf(w, "%s: FAILED at %s: %v\n", v.Tag, o.FailedAt, o.Err)
	}
	if o.Run != nil {
		fmt.Fprintf(w, "release run: %s\n", o.Run.HTMLURL)
	}
	for _, n := range o.Next {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// genericPreflight runs the generic release preflight (strict) with the gh
// token, read-only, as an additional gate of check and publish.
func genericPreflight(p *publish.Project, configPath string) func(ctx context.Context, tag string) (*preflight.Report, error) {
	return func(ctx context.Context, tag string) (*preflight.Report, error) {
		out, err := publish.ExecRunner(ctx, "gh", "auth", "token", "--hostname", "github.com")
		if err != nil {
			return nil, fmt.Errorf("gh auth token: %w", err)
		}
		token := strings.TrimSpace(string(out))
		c, err := github.New(github.Options{Token: token, UserAgent: "hephaestus-publish"})
		if err != nil {
			return nil, err
		}
		in := preflight.Input{Tag: tag, Config: p.Cfg, ConfigPath: configPath, TokenPresent: token != "",
			Git: preflight.GitCLI{Dir: p.Cfg.Root}, GitHub: c}
		return preflight.Run(ctx, in, true, "publish")
	}
}
