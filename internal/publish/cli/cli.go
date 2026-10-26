// Package cli is the command line of publish: argument and flag handling,
// the wiring of internal/publish to git, gh and the generic preflight, and
// the summary it prints. `hephaestus publish` and this repository's own
// release tool (internal/cmd/publish, run by `make publish`) both run it;
// they differ only in their Config.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/preflight"
	"github.com/magicdrive/hephaestus/internal/publish"
)

// Environment variables a Makefile passes the version through (with
// -from-make): values never pass through a shell command line.
const (
	EnvArgs    = "HEPHAESTUS_PUBLISH_ARGS"    // the make goals after the publish target
	EnvVersion = "HEPHAESTUS_PUBLISH_VERSION" // VERSION= on the make command line
	EnvTag     = "HEPHAESTUS_PUBLISH_TAG"     // tag= on the make command line
)

// Command is one publish command.
type Command struct{ Name, Summary string }

// Commands are the commands besides publish itself, in usage order. Shell
// completion offers them.
var Commands = []Command{
	{"check", "Preflight only: no tag, no push, no GitHub writes"},
	{"status", "Show what exists for a version (read-only)"},
	{"watch", "Follow the release run of an already pushed tag and verify it"},
	{"plan", "Print what the release workflow does for a tag (for $GITHUB_OUTPUT)"},
	{"help", "Show how publishing works and its safety rules"},
}

// Config is how one program presents publish.
type Config struct {
	// Name prefixes error messages: "publish" or "hephaestus publish".
	Name string
	// Usage is printed, followed by the flags, on a usage error and for
	// -h after a command; Help is printed by the help command (and, without
	// ImpliedPublish, by -h alone).
	Usage, Help string
	// ImpliedPublish makes publish the command when the first argument is
	// not a command name: hephaestus publish [flags] v1.2.3.
	ImpliedPublish bool
	// DirectHints makes the next steps publish prints name "<Name> watch"
	// and so on, except with -from-make, where they name the make targets
	// (make publish-watch). Without it they always name the make targets.
	DirectHints bool
	// Stdin answers the confirmation prompt (default os.Stdin).
	Stdin *os.File
	// Getenv reads the -from-make variables (default os.Getenv).
	Getenv func(string) string
	// Redact, when set, is given the gh token so that it is hidden from
	// any output.
	Redact func(secret string)
	// Gate, when set, is an extra check of publish and check, made right
	// after the version is resolved; an error stops them with exit code 1
	// before anything else is read.
	Gate func(configPath string, v publish.Version) error
}

// Flags are publish's flags.
type Flags struct {
	Config          string
	Branch          string
	Yes             bool
	FromMake        bool
	DiscoverTimeout time.Duration
	WatchTimeout    time.Duration
	Poll            time.Duration
}

// NewFlagSet defines publish's flags; shell completion reads the same
// definitions.
func NewFlagSet(name string) (*flag.FlagSet, *Flags) {
	f := &Flags{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&f.Config, "config", "hephaestus.yaml", "configuration file")
	fs.StringVar(&f.Branch, "branch", "", "the release branch, the only branch releases are cut from (default: preflight.branch, else the release repository's default branch)")
	fs.BoolVar(&f.Yes, "yes", false, "confirm without a prompt (explicit non-interactive mode)")
	fs.BoolVar(&f.FromMake, "from-make", false, "read the version from the environment the Makefile sets")
	fs.DurationVar(&f.DiscoverTimeout, "discover-timeout", 5*time.Minute, "how long to wait for the release run to appear")
	fs.DurationVar(&f.WatchTimeout, "watch-timeout", 90*time.Minute, "how long to wait for the release run to finish")
	fs.DurationVar(&f.Poll, "poll", 15*time.Second, "polling interval")
	return fs, f
}

// isCommand reports whether name is a command Run dispatches.
func isCommand(name string) bool {
	if name == "publish" {
		return true
	}
	for _, c := range Commands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// impliesPublish reports whether a first argument that is not a command
// starts the publish command's arguments: a flag or something meant as a
// version (v1.2.3, or 1.2.3, which is then rejected with a hint).
func impliesPublish(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return true
	}
	s := strings.TrimPrefix(arg, "v")
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// Run runs publish with args (the words after the program and, for
// hephaestus, after "publish") and returns the exit code: 0 done, 1
// failed, 2 usage, 3 outcome unknown (monitoring stopped).
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, c Config) int {
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, c.Usage)
		return 2
	}
	cmd := args[0]
	switch {
	case cmd == "help" || !c.ImpliedPublish && (cmd == "-h" || cmd == "--help"):
		// With ImpliedPublish, -h is a flag of the publish command: it
		// prints the usage and flags, as every hephaestus command does.
		fmt.Fprint(stdout, c.Help)
		return 0
	case isCommand(cmd):
		args = args[1:]
	case c.ImpliedPublish && impliesPublish(cmd):
		cmd = "publish"
	}
	fs, f := NewFlagSet("publish " + cmd)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, c.Usage); fs.PrintDefaults() }
	if !isCommand(cmd) {
		fmt.Fprintf(stderr, "%s: unknown command %q\n", c.Name, cmd)
		fs.Usage()
		return 2
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	hint := func(command string) string {
		if command == "publish" {
			return c.Name
		}
		return c.Name + " " + command
	}
	if !c.DirectHints || f.FromMake {
		hint = nil // the make targets
	}
	a := publish.Args{Positional: fs.Args()}
	if f.FromMake {
		if len(fs.Args()) > 0 {
			fmt.Fprintf(stderr, "%s: -from-make takes the version from the environment only\n", c.Name)
			return 2
		}
		a = publish.Args{Positional: strings.Fields(c.Getenv(EnvArgs)), VersionVar: c.Getenv(EnvVersion), TagVar: c.Getenv(EnvTag)}
	} else if hint != nil && len(a.Positional) == 0 {
		fmt.Fprintf(stderr, "%s: a version is required: %s v1.2.3\n", c.Name, hint(cmd))
		return 2
	}
	raw, err := publish.ResolveArgs(a)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", c.Name, err)
		return 2
	}
	v, err := publish.ParseVersion(raw)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", c.Name, err)
		return 2
	}
	if c.Gate != nil && (cmd == "publish" || cmd == "check") {
		if err := c.Gate(f.Config, v); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", c.Name, err)
			return 1
		}
	}
	p, err := publish.LoadProject(f.Config)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", c.Name, err)
		return 1
	}
	pl, err := p.Plan(v)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", c.Name, err)
		return 1
	}
	if cmd == "plan" {
		return printPlan(stdout, pl)
	}
	p.BranchFlag = f.Branch // "": preflight.branch, else the default branch

	d := &publish.Deps{
		Git: publish.GitCLI{Run: publish.ExecRunner},
		GH:  publish.GHCLI{Run: publish.ExecRunner, Sleep: publish.Sleep},
		Out: stdout, In: c.Stdin, Interactive: isTerminal(c.Stdin),
		Now: time.Now, Sleep: publish.Sleep, Hint: hint,
	}
	d.Generic = genericPreflight(p, f.Config, c.Redact)
	return execute(ctx, cmd, d, p, pl, publish.Options{Yes: f.Yes, DiscoverTimeout: f.DiscoverTimeout, WatchTimeout: f.WatchTimeout, Poll: f.Poll}, stderr, c.Name)
}

// execute runs check, status, publish or watch with d.
func execute(ctx context.Context, cmd string, d *publish.Deps, p *publish.Project, pl *publish.Plan, opts publish.Options, stderr io.Writer, name string) int {
	v := pl.Version
	switch cmd {
	case "check":
		rep := publish.Preflight(ctx, d, p, pl)
		rep.Print(d.Out)
		if !rep.OK() {
			fmt.Fprintf(d.Out, "\npreflight FAILED for %s\n", v.Tag)
			return 1
		}
		fmt.Fprintf(d.Out, "\npreflight passed: %s %s would tag %s\n", d.Command("publish"), v.Tag, rep.Commit)
		return 0
	case "status":
		s, err := publish.Observe(ctx, d, p, pl)
		if err != nil {
			fmt.Fprintf(stderr, "%s: status: %v\n", name, err)
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
	summarize(d.Out, v, o)
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
func genericPreflight(p *publish.Project, configPath string, redact func(string)) func(ctx context.Context, tag string) (*preflight.Report, error) {
	return func(ctx context.Context, tag string) (*preflight.Report, error) {
		out, err := publish.ExecRunner(ctx, "gh", "auth", "token", "--hostname", "github.com")
		if err != nil {
			return nil, fmt.Errorf("gh auth token: %w", err)
		}
		token := strings.TrimSpace(string(out))
		if redact != nil {
			redact(token)
		}
		c, err := github.New(github.Options{Token: token, UserAgent: "hephaestus-publish"})
		if err != nil {
			return nil, err
		}
		// The release branch the publish preflight resolved (it runs
		// first), so that both preflights check the same branch.
		in := preflight.Input{Tag: tag, Config: p.Cfg, ConfigPath: configPath, TokenPresent: token != "", Branch: p.Branch,
			Git: preflight.GitCLI{Dir: p.Cfg.Root}, GitHub: c}
		return preflight.Run(ctx, in, true, "publish")
	}
}

// CommandUsage is the usage of `hephaestus publish`.
var CommandUsage = func() string {
	var b strings.Builder
	b.WriteString(`usage: hephaestus publish [command] [flags] <version>

Without a command, releases the version: preflight, confirmation, annotated
tag, tag push, then it follows the release workflow run and verifies the
release. Normally run through make: make publish vX.Y.Z (docs/publish.md).

Commands:
`)
	for _, c := range Commands {
		fmt.Fprintf(&b, "  %-8s %s\n", c.Name, c.Summary)
	}
	b.WriteString(`
Versions are release tags: vMAJOR.MINOR.PATCH[-PRERELEASE] (SemVer, a leading
"v", no build metadata), e.g. v1.2.3 or v0.1.0-rc.1. Flags come before the
version.

Flags:
`)
	return b.String()
}()

// CommandHelp is the help of `hephaestus publish`. It describes any
// project: the conventions it names are in docs/publish.md.
const CommandHelp = `hephaestus publish vX.Y.Z (make publish vX.Y.Z) releases <project.name> of
hephaestus.yaml to its release repository (release.github). The release
workflow, .github/workflows/release.yml, is the only publisher: publish pushes
the tag that starts it, then follows it and verifies what it published.

  1. preflight (read-only, plus a fetch of origin/<branch>): the release
     branch is -branch, else preflight.branch, else the release repository's
     default branch on GitHub (main, master or any name); it must exist on
     GitHub, and origin/HEAD, when set, must name the default branch: else
     nothing is tagged. On that branch, clean worktree, HEAD = origin/<branch>
     = <branch> on GitHub, origin is the release repository, the tag exists
     neither locally nor on origin, CI (ci.yml push run on the release branch)
     succeeded on exactly this commit, the v* tag ruleset is active, a ruleset
     on the release branch requires ci-gate, environment
     "release" deploys only from v* tags, no release or draft exists yet, and
     hephaestus preflight --strict passes; for a stable version with a formula
     also the tap: token secret present (name only), pull request mode, formula
     directory, no stale branch or pull request
  2. confirmation (default No; -yes, or YES=1 through make, for an explicit
     non-interactive run)
  3. annotated tag at the checked commit, re-check, then a plain (never forced)
     push of that one tag. The push is the only release trigger and cannot be
     taken back: the tag is never moved or deleted afterwards.
  4. the release.yml run for this tag and commit is found and followed
  5. verification: release published, prerelease flag, "latest", exactly the
     archives and checksum file the configuration names, SHA-256 against the
     checksum file, provenance attestation of every asset (signer workflow, tag
     ref, commit, GitHub-hosted runner), and the tap: untouched when the formula
     is skipped, a formula pull request when it is published (never merged by
     publish)

A prerelease (v1.2.3-rc.1) with brews skip_upload: auto never updates the
Homebrew tap: the release job's tap steps must be skipped, so the tap token is
not even passed to them.

If monitoring stops (network, Ctrl-C, timeout) the release keeps running on
GitHub: hephaestus publish watch vX.Y.Z (make publish-watch vX.Y.Z) resumes,
hephaestus publish status vX.Y.Z (make publish-status vX.Y.Z) shows where it
stands. Conventions, setup and recovery: docs/publish.md.
`
