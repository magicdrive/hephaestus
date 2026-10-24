package hephaestus

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/status"
	"github.com/magicdrive/hephaestus/internal/version"
)

// Exit codes of `hephaestus status`.
const (
	statusExitOK       = 0 // read; no conflict
	statusExitConflict = 1 // an asset conflicts, or the immutable release is incomplete
	statusExitUsage    = 2
	statusExitUnread   = 3 // GitHub could not be read
)

// statusCmd runs `hephaestus status --tag vX.Y.Z`: where the release of a
// tag stands and the next safe action (internal/status). Read-only.
func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer, red *redactor) int {
	var tag, configPath, repoFlag, format, tokenEnv string
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&tag, "tag", "", "the release tag, e.g. v1.2.3 (required)")
	fs.StringVar(&configPath, "config", "", "configuration file (default: hephaestus.yaml in the current directory)")
	fs.StringVar(&repoFlag, "repo", "", "owner/name of the release repository (default: config, then $GITHUB_REPOSITORY)")
	fs.StringVar(&format, "format", "text", "output format: text or json")
	fs.StringVar(&tokenEnv, "token-env", "GITHUB_TOKEN", "environment variable holding a GitHub token for reading (GH_TOKEN is used when it is empty)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return statusExitOK
		}
		return statusExitUsage
	}
	if fs.NArg() > 0 || tag == "" || (format != "text" && format != "json") {
		fmt.Fprintln(stderr, "usage: hephaestus status --tag vX.Y.Z [--config FILE] [--repo owner/name] [--format text|json]")
		return statusExitUsage
	}
	info, err := version.Parse(tag)
	if err != nil {
		fmt.Fprintf(stderr, "hephaestus status: %v\n", err)
		return statusExitUsage
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return statusExitUsage
	}
	_, literals := literalTokens(cfg)
	red.add(literals...)
	owner, repo, err := resolveRepo(cfg, repoFlag)
	if err != nil || owner == "" {
		fmt.Fprintf(stderr, "hephaestus status: the release repository is unknown (%v): set release.github or --repo\n", err)
		return statusExitUsage
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	red.add(token)
	c, err := github.New(github.Options{Token: token, APIURL: apiURL(cfg), UploadURL: uploadURL(cfg), UserAgent: "hephaestus/" + Version})
	if err != nil {
		fmt.Fprintf(stderr, "hephaestus status: %v\n", err)
		return statusExitUsage
	}

	in := status.Input{Config: cfg, Owner: owner, Repo: repo, Tag: tag, GitHub: c,
		Vars: templateVars(cfg, info, "", os.Environ(), tokenEnv, "GH_TOKEN")}
	root, err := os.OpenRoot(cfg.Root)
	if err == nil {
		m, merr := manifest.ReadFileIn(root, cfg.Manifest)
		_ = root.Close()
		switch {
		case merr == nil && m.Project == cfg.Project.Name && m.Version == info.Version:
			in.Manifest = m
		case merr == nil:
			in.ManifestNote = fmt.Sprintf("other-version (%s %s): assets are not compared", m.Project, m.Version)
		default:
			in.ManifestNote = "absent: assets are not compared"
		}
	}
	rep, err := status.Run(ctx, in)
	if err != nil {
		fmt.Fprintf(stderr, "hephaestus status: %v\n", err)
		return statusExitUnread
	}
	if format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return statusExitUnread
		}
	} else {
		rep.WriteText(stdout)
	}
	if rep.State == status.Conflict {
		return statusExitConflict
	}
	return statusExitOK
}
