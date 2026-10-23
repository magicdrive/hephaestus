// Package cli implements the hephaestus command.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/pipeline"
)

// Version is set at link time (-ldflags "-X .../cli.Version=v1.2.3").
var Version = "dev"

const usage = `hephaestus — forge release artifacts into reliable distributions

Usage:
  hephaestus <command> [flags]

Commands:
  check       Validate the configuration and print the resolved plan
  run         Run stages (default: every configured stage)
  build       Run the build stage only
  archive     Run the archive stage only
  checksum    Run the checksum stage only
  release     Run the GitHub release stage only
  formula     Run the Homebrew formula stage only
  tap         Run the Homebrew tap publication stage only
  version     Print the version

Run "hephaestus <command> -h" for flags.
`

// Main runs the CLI and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var stages string
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "hephaestus", Version)
		return 0
	case "check":
	case "run":
	case "build", "archive", "checksum", "release", "formula", "tap":
		stages = cmd
	default:
		fmt.Fprintf(stderr, "hephaestus: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	opts := &Options{Stdout: stdout, Stderr: stderr}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.ConfigPath, "config", "", "configuration file (default: hephaestus.yaml in the current directory)")
	fs.StringVar(&opts.Tag, "tag", "", "release tag (default: $GITHUB_REF_NAME on tag pushes)")
	fs.StringVar(&opts.Commit, "commit", "", "commit SHA (default: $GITHUB_SHA)")
	fs.BoolVar(&opts.Snapshot, "snapshot", false, "build without a tag (version 0.0.0-SNAPSHOT); release and tap stages are refused")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "plan release and tap publication without writing to GitHub")
	fs.BoolVar(&opts.Fresh, "fresh", false, "ignore an existing manifest instead of continuing from it")
	fs.StringVar(&opts.Repo, "repo", "", "owner/name of the release repository (default: config, then $GITHUB_REPOSITORY)")
	fs.StringVar(&opts.TokenEnv, "token-env", "GITHUB_TOKEN", "environment variable holding the release token")
	fs.StringVar(&opts.TapTokenEnv, "tap-token-env", "HOMEBREW_TAP_GITHUB_TOKEN", "environment variable holding the tap token")
	if cmd == "run" {
		fs.StringVar(&stages, "stages", "", "comma-separated stages to run, or all (default: every configured stage)")
	}
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

	cfg, err := loadConfig(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	if cmd == "check" {
		return check(cfg, opts)
	}
	var list []pipeline.Stage
	if stages == "" {
		list = configuredStages(cfg)
		if len(list) == 0 {
			fmt.Fprintln(stderr, "error: the configuration declares no stages")
			return 1
		}
	} else if list, err = pipeline.ParseStages(stages); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	opts.Stages = list
	return run(ctx, cfg, opts)
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
	return config.LoadFile(filepath.Clean(path))
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
		fmt.Fprintf(opts.Stdout, "formula %s → %s/%s\n", b.Name, b.Repository.Owner, b.Repository.Name)
	}
	return 0
}
