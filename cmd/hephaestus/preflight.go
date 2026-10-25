package hephaestus

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/ghaction"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/preflight"
)

// stringList is a repeatable flag that also accepts comma-separated values.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// preflightArgs holds the flags of `hephaestus preflight`.
type preflightArgs struct {
	in                           preflight.Input
	format, tokenEnv, reportPath string
	strict                       bool
	secrets, skips               stringList
}

// flags defines the preflight flags on a; shell completion reads the same
// definitions.
func (a *preflightArgs) flags() *flag.FlagSet {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.StringVar(&a.in.Tag, "tag", "", "the tag to be released, e.g. v1.2.3 (required)")
	fs.StringVar(&a.in.ConfigPath, "config", "", "configuration file (default: hephaestus.yaml in the current directory)")
	fs.StringVar(&a.in.Repo, "repo", "", "owner/name of the release repository (default: release.github, then the git remote)")
	fs.StringVar(&a.format, "format", "text", "output format: text or json")
	fs.StringVar(&a.reportPath, "report", "", "also write the JSON report to this file")
	fs.BoolVar(&a.strict, "strict", false, "treat WARN and UNKNOWN as failures")
	fs.BoolVar(&a.in.Offline, "offline", false, "skip every check that needs the network (GitHub API, git ls-remote)")
	fs.StringVar(&a.in.Remote, "remote", "", "git remote releases are pushed to (default: preflight.remote, then origin)")
	fs.StringVar(&a.in.Branch, "branch", "", "branch releases are tagged on (default: preflight.branch, then the GitHub default branch)")
	fs.StringVar(&a.in.Workflow, "workflow", "", "release workflow file name, e.g. release.yml (default: preflight.workflow)")
	fs.StringVar(&a.in.Environment, "environment", "", "deployment environment of the publishing job (default: preflight.environment)")
	fs.StringVar(&a.in.TapTokenSecret, "tap-token-secret", "", "secret name of the tap token (default: preflight.tap_token_secret, then "+preflight.DefaultTapTokenSecret+")")
	fs.Var(&a.secrets, "secret", "secret name that must exist (repeatable); values are never read")
	fs.Var(&a.skips, "skip", "check IDs to skip (repeatable or comma-separated)")
	fs.StringVar(&a.tokenEnv, "token-env", "GITHUB_TOKEN", "environment variable holding a GitHub token for reading (GH_TOKEN is used when it is empty)")
	return fs
}

// preflightCmd runs `hephaestus preflight`: read-only release safety checks
// before a tag is pushed (internal/preflight). Unlike the other commands it
// does not stop on an invalid configuration: config.load reports it and the
// git and GitHub checks still run.
func preflightCmd(ctx context.Context, args []string, stdout, stderr io.Writer, red *redactor) int {
	var a preflightArgs
	fs := a.flags()
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "\nChecks, in order: %s\n", strings.Join(preflight.Checks(), ", "))
			return 0
		}
		return preflight.ExitUsage
	}
	in, format, tokenEnv, reportPath, strict := a.in, a.format, a.tokenEnv, a.reportPath, a.strict
	secrets, skips := a.secrets, a.skips
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hephaestus preflight: unexpected argument %q\n", fs.Arg(0))
		return preflight.ExitUsage
	}
	if in.Tag == "" {
		fmt.Fprintln(stderr, "hephaestus preflight: --tag is required (the tag you are about to push)")
		return preflight.ExitUsage
	}
	if !slices.Contains(outputFormats, format) {
		fmt.Fprintln(stderr, "hephaestus preflight: --format must be text or json")
		return preflight.ExitUsage
	}
	in.Secrets, in.Skip = secrets, skips

	token := os.Getenv(tokenEnv)
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	red.add(token)
	in.TokenPresent = token != ""

	if in.ConfigPath == "" {
		if p, err := config.Find("."); err == nil {
			in.ConfigPath = p
		} else {
			in.ConfigErr = err
		}
	}
	if in.ConfigPath != "" {
		in.Config, in.ConfigErr = config.LoadFile(in.ConfigPath)
	}
	dir := "."
	if in.Config != nil {
		dir = in.Config.Root
		_, literals := literalTokens(in.Config)
		red.add(literals...)
	}
	in.Git = preflight.GitCLI{Dir: dir}
	if !in.Offline {
		opts := github.Options{Token: token, UserAgent: "hephaestus/" + Version}
		if in.Config != nil {
			opts.APIURL, opts.UploadURL = apiURL(in.Config), uploadURL(in.Config)
		}
		c, err := github.New(opts)
		if err != nil {
			fmt.Fprintf(stderr, "hephaestus preflight: %v\n", err)
			return preflight.ExitUsage
		}
		in.GitHub = c
	}

	rep, err := preflight.Run(ctx, in, strict, Version)
	if err != nil {
		fmt.Fprintf(stderr, "hephaestus preflight: %v\n", err)
		return preflight.ExitUsage
	}
	if reportPath != "" {
		if err := writeReport(reportPath, rep, red); err != nil {
			fmt.Fprintf(stderr, "hephaestus preflight: --report: %v\n", err)
			return preflight.ExitFail
		}
	}
	if format == "json" {
		if err := rep.WriteJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "hephaestus preflight: %v\n", err)
			return preflight.ExitFail
		}
		return rep.ExitCode()
	}
	rep.WriteText(stdout)
	// In GitHub Actions: annotations on the run page and a summary table.
	// Annotations are workflow commands on stdout, so never with JSON.
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		var rows [][]string
		for _, c := range rep.Checks {
			// Through stdout, which redacts known secrets.
			switch c.Status {
			case preflight.Fail:
				fmt.Fprint(stdout, ghaction.Annotation("error", "preflight "+c.ID, c.Message))
			case preflight.Warn, preflight.Unknown:
				fmt.Fprint(stdout, ghaction.Annotation("warning", "preflight "+c.ID+" "+string(c.Status), c.Message))
			}
			rows = append(rows, []string{string(c.Status), c.ID, c.Message})
		}
		_ = ghaction.AppendSummary(red.redact(fmt.Sprintf("### Release preflight: %s %s — %s\n\n%s\n", rep.Repository, rep.Tag, strings.ToUpper(rep.Status),
			ghaction.Table([]string{"Status", "Check", "Detail"}, rows))))
	}
	return rep.ExitCode()
}

// writeReport writes the JSON report to path (created or truncated), with
// known secrets redacted like every other output.
func writeReport(path string, rep *preflight.Report, red *redactor) error {
	var b strings.Builder
	if err := rep.WriteJSON(&b); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(red.redact(b.String())), 0o644) //nolint:gosec // G306: a report, not a secret
}
