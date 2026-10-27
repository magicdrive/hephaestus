// Package hephaestus implements the hephaestus command line.
package hephaestus

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/magicdrive/hephaestus/internal/compat/goreleaser"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pipeline"
	"github.com/magicdrive/hephaestus/internal/tmpl"
	"github.com/magicdrive/hephaestus/internal/workflowcheck"
)

// Version is the running hephaestus version, set by Execute.
var Version = "dev"

// Main runs the CLI and returns the process exit code.
//
// Everything it writes passes through a redactor that replaces the known
// secrets (the tokens, a rendered repository.token, tokens written in the
// configuration) with "***".
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	red := &redactor{}
	ow, ew := red.writer(stdout), red.writer(stderr)
	defer ow.Flush()
	defer ew.Flush()
	return mainCmd(ctx, args, ow, ew, red)
}

func mainCmd(ctx context.Context, args []string, stdout, stderr io.Writer, red *redactor) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var stages, phase string
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "hephaestus", Version)
		return 0
	case "preflight":
		return preflightCmd(ctx, rest, stdout, stderr, red)
	case "status":
		return statusCmd(ctx, rest, stdout, stderr, red)
	case "publish":
		return publishCmd(ctx, rest, stdout, stderr, red)
	case "init":
		return initCmd(ctx, rest, stdout, stderr)
	case "completion":
		return completionCmd(rest, stdout, stderr)
	case "__complete":
		return completeCmd(rest, stdout)
	case "check":
	case "run":
	case "build", "archive", "checksum", "release", "formula", "tap":
		stages = cmd
	default:
		fmt.Fprintf(stderr, "hephaestus: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	opts := &Options{Stdout: stdout, Stderr: stderr, redact: red}
	fs := runFlags(cmd, opts, &stages, &phase)
	fs.SetOutput(stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hephaestus %s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2
	}
	if phase != "" && phase != phaseLocal && phase != phasePublish {
		fmt.Fprintf(stderr, "hephaestus %s: --phase must be %s or %s\n", cmd, phaseLocal, phasePublish)
		return 2
	}
	red.addEnviron(os.Environ(), opts.TokenEnv, opts.TapTokenEnv)

	cfg, err := loadConfig(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	tokenWarnings, literals := literalTokens(cfg)
	red.add(literals...)
	// Variables read by repository.token are credentials too.
	red.addEnviron(os.Environ(), tokenRefs(cfg)...)
	for _, w := range append(cfg.Warnings(), tokenWarnings...) {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	if cmd == "check" {
		return check(cfg, opts)
	}
	var list []pipeline.Stage
	if stages == "" {
		list = configuredStages(cfg)
		if opts.Snapshot {
			// A snapshot never publishes and has no tag: without explicit
			// stages it runs every configured stage except release, formula
			// (its URLs and version come from the tag) and tap.
			list = slices.DeleteFunc(list, needsTag)
		}
		if len(list) == 0 {
			fmt.Fprintln(stderr, "error: the configuration declares no stages")
			return 1
		}
	} else if list, err = pipeline.ParseStages(stages); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if phase != "" {
		list = inPhase(list, phase)
		if len(list) == 0 {
			fmt.Fprintf(stderr, "==> phase %s: no stages to run\n", phase)
			return 0
		}
	}
	opts.Stages = list
	return run(ctx, cfg, opts)
}

// runFlags defines the flags of check, run and the single-stage commands;
// stages and phase are set only by run. Shell completion reads the same
// definitions.
func runFlags(cmd string, opts *Options, stages, phase *string) *flag.FlagSet {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&opts.ConfigPath, "config", "", "configuration file (default: hephaestus.yaml in the current directory)")
	fs.StringVar(&opts.Tag, "tag", "", "release tag (default: $GITHUB_REF_NAME on tag pushes)")
	fs.StringVar(&opts.Commit, "commit", "", "commit SHA (default: $GITHUB_SHA)")
	fs.BoolVar(&opts.Snapshot, "snapshot", false, "build without a tag (version 0.0.0-SNAPSHOT); configured release, formula and tap stages are skipped, and refused when named in --stages")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "plan release and tap publication without writing to GitHub")
	fs.BoolVar(&opts.Fresh, "fresh", false, "ignore an existing manifest instead of continuing from it")
	fs.StringVar(&opts.Repo, "repo", "", "owner/name of the release repository (default: config, then $GITHUB_REPOSITORY)")
	fs.StringVar(&opts.TokenEnv, "token-env", "GITHUB_TOKEN", "environment variable holding the release token")
	fs.StringVar(&opts.TapTokenEnv, "tap-token-env", "HOMEBREW_TAP_GITHUB_TOKEN", "environment variable holding the tap token")
	if cmd == "check" {
		fs.BoolVar(&opts.Workflows, "workflows", false, "also check the release workflow's conventions (docs/preflight.md#release-workflow); a violation exits 1")
	}
	if cmd == "run" {
		fs.StringVar(stages, "stages", "", "comma-separated stages to run, or all (default: every configured stage)")
		fs.StringVar(phase, "phase", "", "run only the selected stages of one phase: local (build, archive, checksum) or publish (release, formula, tap)")
	}
	return fs
}

// Phases split a run so that build code and publishing credentials need not
// share a process: the GitHub Action runs the local phase in a step without
// tokens and the publish phase in a later step.
const (
	phaseLocal   = "local"   // build, archive, checksum
	phasePublish = "publish" // release, formula, tap
)

// inPhase returns the stages of list that belong to phase, in order.
func inPhase(list []pipeline.Stage, phase string) []pipeline.Stage {
	var out []pipeline.Stage
	for _, s := range list {
		local := s == pipeline.Build || s == pipeline.Archive || s == pipeline.Checksum
		if local == (phase == phaseLocal) {
			out = append(out, s)
		}
	}
	return out
}

// tokenRefs returns the environment variables brews[].repository.token
// templates read. They hold credentials whatever their name, so they are
// hidden from .Env, removed from build commands and redacted.
func tokenRefs(cfg *config.Config) []string {
	var names []string
	for _, b := range cfg.Brews {
		refs, err := tmpl.EnvRefs(b.Repository.Token)
		if err != nil {
			continue // reported by configuration validation
		}
		for _, r := range refs {
			if !slices.Contains(names, r) {
				names = append(names, r)
			}
		}
	}
	return names
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		if path, err = config.Find(wd); err != nil {
			return nil, err
		}
	}
	cfg, err := config.LoadFile(filepath.Clean(path))
	if err != nil || cfg.BrewsFrom == "" {
		return cfg, err
	}
	return cfg, loadBrewsFrom(cfg)
}

// loadBrewsFrom appends the brews of the GoReleaser configuration named by
// brews_from, read through the project root.
func loadBrewsFrom(cfg *config.Config) error {
	f, err := manifest.OpenConfined(cfg.Root, cfg.BrewsFrom)
	if err != nil {
		return fmt.Errorf("brews_from: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, goreleaser.MaxConfigSize+1))
	if err != nil {
		return fmt.Errorf("brews_from: %w", err)
	}
	project, brews, warnings, err := goreleaser.Parse(data)
	if err != nil {
		return fmt.Errorf("brews_from %s: %w", cfg.BrewsFrom, err)
	}
	if project != "" && project != cfg.Project.Name {
		// Formula names default to project_name (as in GoReleaser), so the
		// formula is not renamed; templates still see project.name.
		warnings = append(warnings, fmt.Sprintf("project_name %q differs from project.name %q: formulae without a name are named %q as in GoReleaser, but {{ .ProjectName }} in templates (commit message, pull request branch) is %q", project, cfg.Project.Name, project, cfg.Project.Name))
	}
	for i := range warnings {
		warnings[i] = "brews_from " + cfg.BrewsFrom + ": " + warnings[i]
	}
	if err := cfg.AddBrews(brews, warnings); err != nil {
		return fmt.Errorf("brews_from %s: %w", cfg.BrewsFrom, err)
	}
	return nil
}

// configuredStages returns the stages the configuration declares.
func configuredStages(cfg *config.Config) []pipeline.Stage {
	var out []pipeline.Stage
	if len(cfg.Builds) > 0 {
		out = append(out, pipeline.Build)
	}
	if len(cfg.Archives) > 0 {
		out = append(out, pipeline.Archive)
		if !cfg.Checksum.Disable {
			out = append(out, pipeline.Checksum)
		}
	}
	if cfg.Release.GitHub.IsEnabled() {
		out = append(out, pipeline.Release)
	}
	if len(cfg.Brews) > 0 {
		out = append(out, pipeline.Formula)
		for _, b := range cfg.Brews {
			if b.SkipUpload != "true" {
				out = append(out, pipeline.Tap)
				break
			}
		}
	}
	return out
}

func check(cfg *config.Config, opts *Options) int {
	fmt.Fprintf(opts.Stdout, "configuration OK: project %s, root %s\n", cfg.Project.Name, cfg.Root)
	var names []string
	for _, s := range configuredStages(cfg) {
		names = append(names, string(s))
	}
	fmt.Fprintf(opts.Stdout, "configured stages: %s\n", strings.Join(names, ", "))
	for _, b := range cfg.Builds {
		fmt.Fprintf(opts.Stdout, "build %s (%s)", b.ID, b.Provider())
		if b.Provider() != config.ProviderGoReleaser {
			var ts []string
			for _, t := range config.ExpandTargets(b.Targets, b.Matrix) {
				ts = append(ts, t.String())
			}
			fmt.Fprintf(opts.Stdout, ": %s", strings.Join(ts, " "))
		}
		fmt.Fprintln(opts.Stdout)
	}
	for _, a := range cfg.Archives {
		fmt.Fprintf(opts.Stdout, "archive %s: %s, %d file entries\n", a.ID, a.Format, len(a.Files))
	}
	for _, b := range cfg.Brews {
		fmt.Fprintf(opts.Stdout, "formula %s → %s/%s: %s\n", b.Name, b.Repository.Owner, b.Repository.Name, homebrew.DescribePublish(&b))
		if len(b.GoReleaserDefaults) > 0 {
			fmt.Fprintf(opts.Stdout, "  GoReleaser defaults (brews_from): %s\n", strings.Join(b.GoReleaserDefaults, "; "))
		}
	}
	if opts.Workflows {
		return checkWorkflows(cfg, opts)
	}
	return 0
}

// checkWorkflows runs the release workflow checks (check --workflows) on
// the repository holding the configuration, for the assets of --tag
// (default v0.0.0: only the names' shape matters). Anything that keeps a
// check from running fails it.
func checkWorkflows(cfg *config.Config, opts *Options) int {
	root, err := repositoryRoot(cfg.Root)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "error: --workflows: %v\n", err)
		return 1
	}
	tag := opts.Tag
	if tag == "" {
		tag = "v0.0.0"
	}
	vars, err := workflowcheck.TagVars(cfg, tag)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "error: --workflows: --tag %q: %v\n", tag, err)
		return 2
	}
	o := workflowcheck.FromConfig(workflowcheck.Options{Dir: root}, cfg, vars)
	fmt.Fprintf(opts.Stdout, "release workflow checks (%s, assets of %s):\n", root, tag)
	code := 0
	for _, r := range workflowcheck.Run(o) {
		fmt.Fprintf(opts.Stdout, "  %-4s  %-22s %s\n", r.Status, r.ID, workflowSummary(r))
		for _, p := range r.Problems[min(1, len(r.Problems)):] {
			fmt.Fprintf(opts.Stdout, "        %-22s %s\n", "", p)
		}
		if r.Status == workflowcheck.Fail {
			code = 1
			fmt.Fprintf(opts.Stdout, "        %-22s → %s\n", "", r.Fix)
		}
	}
	return code
}

func workflowSummary(r workflowcheck.Result) string {
	if len(r.Problems) > 0 {
		return r.Problems[0]
	}
	return r.Message
}

// repositoryRoot is dir or its nearest parent with a .git entry (a
// directory, or a file in a worktree or submodule).
func repositoryRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no git repository holds %s, so its .github/workflows cannot be found", dir)
		}
		d = parent
	}
}
