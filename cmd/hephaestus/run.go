package hephaestus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
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

	// redact replaces known secrets in messages, outputs and the summary.
	redact *redactor
}

// SnapshotVersion is used with --snapshot.
const SnapshotVersion = "0.0.0-SNAPSHOT"

func run(ctx context.Context, cfg *config.Config, opts *Options) int {
	if opts.redact == nil {
		opts.redact = &redactor{}
		opts.redact.addEnviron(os.Environ(), opts.TokenEnv, opts.TapTokenEnv)
	}
	red := opts.redact
	rep := &report{Stages: opts.Stages, DryRun: opts.DryRun, redact: red.redact}
	err := safeExecute(ctx, cfg, opts, rep)
	rep.Err = err
	if err != nil {
		fmt.Fprintf(opts.Stderr, "error: %s\n", red.redact(err.Error()))
	}
	if rep.Manifest != nil {
		for _, a := range rep.Manifest.Artifacts {
			if a.Kind == manifest.KindArchive || a.Kind == manifest.KindChecksum || a.Kind == manifest.KindFormula {
				fmt.Fprintf(opts.Stderr, "    %s  %s\n", a.SHA256, a.Path)
			}
		}
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(opts.Stderr, "warning: %s\n", red.redact(w))
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

// safeExecute runs execute and turns a panic into an error, so that a bug in
// a stage still ends in status "failed" with outputs and a summary instead
// of a crash that leaves the workflow without them.
func safeExecute(ctx context.Context, cfg *config.Config, opts *Options, rep *report) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(opts.Stderr, "panic: %v\n%s", r, debug.Stack())
			if rep.Failed == "" && len(rep.Ran) > 0 {
				rep.Failed = rep.Ran[len(rep.Ran)-1]
			}
			err = fmt.Errorf("internal error (please report it): %v", r)
			if rep.Failed != "" {
				err = fmt.Errorf("stage %s: %w", rep.Failed, err)
			}
		}
	}()
	return executeFn(ctx, cfg, opts, rep)
}

// executeFn is execute, replaceable in tests.
var executeFn = execute

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
	refs := tokenRefs(cfg)
	credVars := append([]string{opts.TokenEnv, opts.TapTokenEnv}, refs...)
	vars := templateVars(cfg, info, commit, os.Environ(), credVars...)

	owner, repo, err := resolveRepo(cfg, opts.Repo)
	if err != nil {
		return err
	}
	token := os.Getenv(opts.TokenEnv)
	tapToken := os.Getenv(opts.TapTokenEnv)
	ghaction.MaskValue(token)
	ghaction.MaskValue(tapToken)
	opts.redact.add(token, tapToken)
	// No rendered (published) value may contain a token: a formula, release
	// notes, a commit message or a pull request title.
	tmpl.RegisterSecret(token, tapToken)
	for _, r := range refs {
		v := os.Getenv(r)
		ghaction.MaskValue(v)
		opts.redact.add(v)
		tmpl.RegisterSecret(v)
	}
	if slices.Contains(opts.Stages, pipeline.Build) && (token != "" || tapToken != "") &&
		(slices.Contains(opts.Stages, pipeline.Release) || slices.Contains(opts.Stages, pipeline.Tap)) {
		w := "build commands run with access to this process's credentials (the release or tap token is set): " +
			"they are removed from the commands' environment, but code running as the same user may still read them; " +
			"run build and publish in separate jobs: docs/github-action.md#separate-build-and-publish-jobs"
		fmt.Fprintf(opts.Stderr, "warning: %s\n", w)
		ghaction.Warning(w)
		rep.Warnings = append(rep.Warnings, w)
	}

	manifestPath := filepath.Join(cfg.Root, filepath.FromSlash(cfg.Manifest))
	m, err := loadManifest(manifestPath, cfg, info, commit, opts.Fresh)
	if err != nil {
		return err
	}
	rep.Manifest = m

	facts := pipeline.Facts{
		Manifest: m, HasBuilds: len(cfg.Builds) > 0, BuildsArchives: buildsImportArchives(cfg), HasArchives: len(cfg.Archives) > 0,
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

	save := func() error {
		root, err := os.OpenRoot(cfg.Root)
		if err != nil {
			return err
		}
		defer root.Close()
		return m.WriteFileIn(root, cfg.Manifest)
	}
	newClient := func(tok string) (*github.Client, error) {
		return github.New(github.Options{Token: tok, APIURL: apiURL(cfg), UploadURL: uploadURL(cfg), UserAgent: "hephaestus/" + Version})
	}
	// Validate the API and upload URLs before any stage runs when a stage
	// talks to GitHub, so a bad URL is reported as such up front.
	if slices.Contains(opts.Stages, pipeline.Release) || slices.Contains(opts.Stages, pipeline.Tap) {
		if _, err := newClient(""); err != nil {
			return err
		}
	}
	var client *github.Client
	if token != "" {
		if client, err = newClient(token); err != nil {
			return fmt.Errorf("%s: %w", opts.TokenEnv, err)
		}
	}
	// clientFor builds a client for a rendered brews[].repository.token,
	// which is then a known secret.
	clientFor := func(tok string) *github.Client {
		opts.redact.add(tok)
		c, err := newClient(tok)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "error: repository.token: %s\n", opts.redact.redact(err.Error()))
			return nil
		}
		return c
	}

	for _, stage := range opts.Stages {
		fmt.Fprintf(opts.Stderr, "==> %s\n", stage)
		rep.Ran = append(rep.Ran, stage)
		var err error
		switch stage {
		case pipeline.Build:
			var res *build.Result
			res, err = build.Run(ctx, build.Input{Config: cfg, Manifest: m, Vars: vars, Environ: os.Environ(), ScrubEnv: credVars, Stdout: opts.Stderr, Stderr: opts.Stderr})
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
				if tc, err = newClient(tapToken); err != nil {
					err = fmt.Errorf("%s: %w", opts.TapTokenEnv, err)
					break
				}
			}
			var res []tap.Result
			res, err = tap.Run(ctx, tap.Input{Config: cfg, Manifest: m, Vars: vars, Client: tc, ClientFor: clientFor, DryRun: opts.DryRun, Log: opts.Stderr})
			rep.Tap = res
			for _, t := range res {
				rep.Warnings = append(rep.Warnings, t.Warnings...)
			}
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

// templateVars builds the template variables. .Env is the process
// environment without credentials: the variables named in hidden (the token
// variables), build.DefaultScrubEnv, and any name tmpl.IsCredentialName
// matches, because most rendered values (release notes, formulas, commit
// messages) are published. Top-level env entries are always visible: writing
// one is an explicit opt-in. The complete environment stays available to
// the values meant to read a credential (tmpl.Vars.Secret).
func templateVars(cfg *config.Config, info version.Info, commit string, environ []string, hidden ...string) tmpl.Vars {
	hide := map[string]bool{}
	for _, n := range append(append([]string(nil), build.DefaultScrubEnv...), hidden...) {
		hide[strings.ToUpper(n)] = true
	}
	env, full := map[string]string{}, map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		full[k] = v
		if !hide[strings.ToUpper(k)] && !tmpl.IsCredentialName(k) {
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
	v := tmpl.Vars{
		Project: cfg.Project.Name, ProjectName: cfg.Project.Name,
		Version: info.Version, Tag: info.Tag, RawVersion: info.Tag,
		Major: info.Major, Minor: info.Minor, Patch: info.Patch, Prerelease: info.Prerelease,
		Commit: commit, ShortCommit: short, Env: env,
	}
	return tmpl.WithSecretEnv(v, full)
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
		root, err := os.OpenRoot(cfg.Root)
		if err != nil {
			return nil, err
		}
		m, err := manifest.ReadFileIn(root, cfg.Manifest)
		_ = root.Close()
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

func buildsImportArchives(cfg *config.Config) bool {
	for i := range cfg.Builds {
		if cfg.Builds[i].ImportsArchives() {
			return true
		}
	}
	return false
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
