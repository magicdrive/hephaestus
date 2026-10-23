package hephaestus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/magicdrive/hephaestus/internal/archive"
	"github.com/magicdrive/hephaestus/internal/build"
	"github.com/magicdrive/hephaestus/internal/checksum"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/ghaction"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pipeline"
	"github.com/magicdrive/hephaestus/internal/release"
	"github.com/magicdrive/hephaestus/internal/tap"
	"github.com/magicdrive/hephaestus/internal/tmpl"
	"github.com/magicdrive/hephaestus/internal/version"
)

// Options are the resolved command-line options.
type Options struct {
	ConfigPath  string
	Tag         string
	Commit      string
	Snapshot    bool
	DryRun      bool
	Fresh       bool
	Repo        string
	TokenEnv    string
	TapTokenEnv string
	Stages      []pipeline.Stage
	Stdout      io.Writer
	Stderr      io.Writer
}

// SnapshotVersion is used with --snapshot.
const SnapshotVersion = "0.0.0-SNAPSHOT"

func run(ctx context.Context, cfg *config.Config, opts *Options) int {
	rep := &report{Stages: opts.Stages, DryRun: opts.DryRun}
	err := execute(ctx, cfg, opts, rep)
	rep.Err = err
	if err != nil {
		fmt.Fprintf(opts.Stderr, "error: %v\n", err)
	}
	if rep.Manifest != nil {
		for _, a := range rep.Manifest.Artifacts {
			if a.Kind == manifest.KindArchive || a.Kind == manifest.KindChecksum || a.Kind == manifest.KindFormula {
				fmt.Fprintf(opts.Stderr, "    %s  %s\n", a.SHA256, a.Path)
			}
		}
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(opts.Stderr, "warning: %s\n", w)
	}
	fmt.Fprintf(opts.Stderr, "==> %s\n", rep.status())
	if werr := rep.publish(); werr != nil {
		fmt.Fprintf(opts.Stderr, "warning: could not write GitHub Actions outputs: %v\n", werr)
	}
	if err != nil {
		return 1
	}
	return 0
}

func execute(ctx context.Context, cfg *config.Config, opts *Options, rep *report) error {
	tag := opts.Tag
	if tag == "" && os.Getenv("GITHUB_REF_TYPE") == "tag" {
		tag = os.Getenv("GITHUB_REF_NAME")
	}
	var info version.Info
	switch {
	case opts.Snapshot && tag != "":
		return errors.New("--snapshot and a tag are mutually exclusive")
	case opts.Snapshot:
		info = version.Info{Version: SnapshotVersion, Major: "0", Minor: "0", Patch: "0", Prerelease: "SNAPSHOT"}
		for _, s := range opts.Stages {
			if s == pipeline.Release || s == pipeline.Tap {
				return fmt.Errorf("stage %s cannot run with --snapshot", s)
			}
		}
	case tag == "":
		return errors.New("a tag is required: pass --tag, run on a tag push, or use --snapshot")
	default:
		var err error
		if info, err = version.Parse(tag); err != nil {
			return err
		}
	}
	rep.Version = info.Version
	commit := opts.Commit
	if commit == "" {
		commit = os.Getenv("GITHUB_SHA")
	}
	vars := templateVars(cfg, info, commit)

	owner, repo, err := resolveRepo(cfg, opts.Repo)
	if err != nil {
		return err
	}
	token := os.Getenv(opts.TokenEnv)
	tapToken := os.Getenv(opts.TapTokenEnv)
	ghaction.MaskValue(token)
	ghaction.MaskValue(tapToken)

	manifestPath := filepath.Join(cfg.Root, filepath.FromSlash(cfg.Manifest))
	m, err := loadManifest(manifestPath, cfg, info, commit, opts.Fresh)
	if err != nil {
		return err
	}
	rep.Manifest = m

	facts := pipeline.Facts{
		Manifest: m, HasBuilds: len(cfg.Builds) > 0, HasArchives: len(cfg.Archives) > 0,
		ArchivesSelfFed:  archivesSelfFed(cfg),
		ChecksumDisabled: cfg.Checksum.Disable, HasGitHubRelease: cfg.Release.GitHub.IsEnabled(),
		HasBrews: len(cfg.Brews) > 0, BrewsHaveURL: brewsHave(cfg, func(b config.Brew) bool { return b.URLTemplate != "" }),
		BrewsHaveTap: brewsHave(cfg, func(b config.Brew) bool { return b.Repository.Owner != "" && b.Repository.Name != "" }),
		SkipTapIf:    brewsHave(cfg, func(b config.Brew) bool { return b.SkipUpload == "true" }),
		Tag:          info.Tag, Token: token != "", DryRun: opts.DryRun,
		TapToken: tapToken != "" || brewsHave(cfg, func(b config.Brew) bool { return b.Repository.Token != "" }),
	}
	if owner != "" {
		facts.Repo = owner + "/" + repo
	}
	if err := pipeline.Check(opts.Stages, facts); err != nil {
		return fmt.Errorf("cannot start:\n%w", err)
	}

	save := func() error { return m.WriteFile(manifestPath) }
	var client *github.Client
	newClient := func(tok string) *github.Client {
		c, err := github.New(github.Options{Token: tok, APIURL: apiURL(cfg), UploadURL: uploadURL(cfg), UserAgent: "hephaestus/" + Version})
		if err != nil {
			return nil
		}
		return c
	}
	if token != "" {
		client = newClient(token)
	}

	for _, stage := range opts.Stages {
		fmt.Fprintf(opts.Stderr, "==> %s\n", stage)
		rep.Ran = append(rep.Ran, stage)
		var err error
		switch stage {
		case pipeline.Build:
			var res *build.Result
			res, err = build.Run(ctx, build.Input{Config: cfg, Manifest: m, Vars: vars, Environ: os.Environ(), Stdout: opts.Stderr, Stderr: opts.Stderr})
			if res != nil {
				rep.Build = res
			}
		case pipeline.Archive:
			var res *archive.Result
			res, err = archive.Run(archive.Input{Config: cfg, Manifest: m, Vars: vars, SourceDateEpoch: os.Getenv("SOURCE_DATE_EPOCH")})
			if res != nil {
				rep.Archive = res
				for name, skipped := range res.Skipped {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: optional files not found: %s", name, strings.Join(skipped, ", ")))
				}
			}
		case pipeline.Checksum:
			_, err = checksum.Run(checksum.Input{Config: cfg, Manifest: m, Vars: vars})
		case pipeline.Release:
			var res *release.Result
			res, err = release.Run(ctx, release.Input{Config: cfg, Manifest: m, Vars: vars, Client: client, Owner: owner, Repo: repo, Tag: info.Tag, DryRun: opts.DryRun, Log: opts.Stderr})
			rep.Release = res
		case pipeline.Formula:
			var res []homebrew.Formula
			res, err = homebrew.Run(homebrew.Input{Config: cfg, Manifest: m, Vars: vars, Owner: owner, Repo: repo, Tag: info.Tag})
			rep.Formulas = res
			for _, f := range res {
				rep.Warnings = append(rep.Warnings, f.Warnings...)
			}
		case pipeline.Tap:
			var tc *github.Client
			if tapToken != "" {
				tc = newClient(tapToken)
			}
			var res []tap.Result
			res, err = tap.Run(ctx, tap.Input{Config: cfg, Manifest: m, Vars: vars, Client: tc, ClientFor: newClient, DryRun: opts.DryRun, Log: opts.Stderr})
			rep.Tap = res
		}
		if serr := save(); serr != nil && err == nil {
			err = fmt.Errorf("save manifest: %w", serr)
		}
		if err != nil {
			rep.Failed = stage
			return fmt.Errorf("stage %s: %w", stage, err)
		}
	}
	return nil
}

func templateVars(cfg *config.Config, info version.Info, commit string) tmpl.Vars {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for _, e := range cfg.Env {
		k, v, _ := strings.Cut(string(e), "=")
		env[k] = v
	}
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	return tmpl.Vars{
		Project: cfg.Project.Name, ProjectName: cfg.Project.Name,
		Version: info.Version, Tag: info.Tag, RawVersion: info.Tag,
		Major: info.Major, Minor: info.Minor, Patch: info.Patch, Prerelease: info.Prerelease,
		Commit: commit, ShortCommit: short, Env: env,
	}
}

func resolveRepo(cfg *config.Config, flagRepo string) (string, string, error) {
	spec := flagRepo
	if spec == "" && cfg.Release.GitHub != nil && cfg.Release.GitHub.Owner != "" {
		spec = cfg.Release.GitHub.Owner + "/" + cfg.Release.GitHub.Repo
	}
	if spec == "" {
		spec = os.Getenv("GITHUB_REPOSITORY")
	}
	if spec == "" {
		return "", "", nil
	}
	owner, name, ok := strings.Cut(spec, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository %q must be owner/name", spec)
	}
	return owner, name, nil
}

func loadManifest(path string, cfg *config.Config, info version.Info, commit string, fresh bool) (*manifest.Manifest, error) {
	if !fresh {
		m, err := manifest.ReadFile(path)
		switch {
		case err == nil:
			if m.Project != cfg.Project.Name || m.Version != info.Version {
				return nil, fmt.Errorf("%s belongs to %s %s, not %s %s; pass --fresh to start over", path, m.Project, m.Version, cfg.Project.Name, info.Version)
			}
			return m, nil
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	return manifest.New(cfg.Project.Name, info.Version, info.Tag, commit), nil
}

func archivesSelfFed(cfg *config.Config) bool {
	for _, a := range cfg.Archives {
		if len(a.Targets) == 0 || !a.FilesDeclared() {
			return false
		}
	}
	return len(cfg.Archives) > 0
}

func brewsHave(cfg *config.Config, pred func(config.Brew) bool) bool {
	for _, b := range cfg.Brews {
		if !pred(b) {
			return false
		}
	}
	return len(cfg.Brews) > 0
}

func apiURL(cfg *config.Config) string {
	if g := cfg.Release.GitHub; g != nil && g.APIURL != "" {
		return g.APIURL
	}
	return os.Getenv("GITHUB_API_URL")
}

func uploadURL(cfg *config.Config) string {
	if g := cfg.Release.GitHub; g != nil {
		return g.UploadURL
	}
	return ""
}

// jsonString renders v compactly for an output value.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
