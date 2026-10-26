package publish

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/archive"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Project is the release setup of the repository being released:
// hephaestus.yaml plus the GitHub names its release workflow relies on.
// The names are conventions every project released with publish follows
// (docs/publish.md), not settings.
type Project struct {
	Cfg  *config.Config
	Repo Repo // where the release is published (release.github)

	// Branch is the only branch releases are cut from: the release
	// repository's default branch (main, master or any other name). Preflight
	// resolves it; set before, it is an explicit choice (-branch) that must
	// name that default branch.
	Branch         string
	ReleaseFile    string // the release workflow, the only publisher
	CIFile         string // the CI workflow that must be green on the commit
	RequiredCheck  string // the status check the main ruleset requires
	Environment    string // the environment of the publishing job
	TapTokenSecret string // the tap token's secret name in Environment
}

// Release workflow facts that publish and the workflow share: the
// conventions of docs/publish.md, the same for every project. For
// Hephaestus itself, repo_publish_test.go keeps .github/workflows/release.yml
// in line with them.
const (
	ReleaseWorkflowFile = "release.yml"
	CIWorkflowFile      = "ci.yml"
	ReleaseEnvironment  = "release"
	TapTokenSecret      = "HOMEBREW_TAP_GITHUB_TOKEN" //nolint:gosec // a secret name, not a credential
	RequiredCheck       = "ci-gate"
	// TapPreflightStep and TapPublishStep are the release job's steps that
	// use the tap token; they run only when the tap is published.
	TapPreflightStep = "Rehearse the formula and tap pull request (read-only)"
	TapPublishStep   = "Generate the formula and open a tap pull request"
)

// LoadProject reads the configuration at path.
func LoadProject(path string) (*Project, error) {
	cfg, err := config.LoadFile(path)
	if err != nil {
		return nil, err
	}
	return NewProject(cfg)
}

// NewProject derives the project from a loaded configuration.
func NewProject(cfg *config.Config) (*Project, error) {
	g := cfg.Release.GitHub
	if cfg.BrewsFrom != "" {
		// publish would see no formula and could not verify the tap.
		return nil, errors.New("brews_from is not supported by publish: write the brews in hephaestus.yaml")
	}
	if !g.IsEnabled() || g.Owner == "" || g.Repo == "" {
		return nil, errors.New("release.github.owner and release.github.name must be set: publish releases to one known repository")
	}
	return &Project{
		Cfg: cfg, Repo: Repo{Owner: g.Owner, Name: g.Repo},
		ReleaseFile: ReleaseWorkflowFile, CIFile: CIWorkflowFile, RequiredCheck: RequiredCheck,
		Environment: ReleaseEnvironment, TapTokenSecret: TapTokenSecret,
	}, nil
}

// Plan is what releasing one version will do, derived from the
// configuration with the same code the pipeline uses.
type Plan struct {
	Version    Version
	Prerelease bool // the GitHub release is marked prerelease
	Draft      bool // release.github.draft: the release stays a draft
	Taps       []TapPlan
	Archives   []string // expected archive asset names, sorted
	Checksum   string   // checksum asset name ("" when disabled)
}

// TapPlan is one formula's publication.
type TapPlan struct {
	Formula string
	Publish homebrew.Publish
	// URLPrefix is the download URL prefix the formula must use.
	URLPrefix string
	// Completions are the install lines of brews[].completions the
	// formula must contain (none when it is unset).
	Completions []string
}

// Vars returns the template variables of version v, as the pipeline sets
// them (without a commit or environment).
func (p *Project) Vars(v Version) tmpl.Vars {
	return tmpl.Vars{
		Project: p.Cfg.Project.Name, ProjectName: p.Cfg.Project.Name,
		Version: v.Version, Tag: v.Tag, RawVersion: v.Tag,
		Major: v.Info.Major, Minor: v.Info.Minor, Patch: v.Info.Patch, Prerelease: v.Info.Prerelease,
		Env: map[string]string{},
	}
}

// Plan resolves what releasing v does.
func (p *Project) Plan(v Version) (*Plan, error) {
	cfg := p.Cfg
	vars := p.Vars(v)
	pl := &Plan{Version: v, Draft: cfg.Release.GitHub.Draft}
	switch cfg.Release.GitHub.Prerelease {
	case "true":
		pl.Prerelease = true
	case "false":
	default:
		pl.Prerelease = v.Prerelease
	}
	for _, k := range cfg.Release.GitHub.Include {
		if k != string(manifest.KindArchive) && k != string(manifest.KindChecksum) {
			return nil, fmt.Errorf("release.github.include %q: publish verifies archives and the checksum file only", k)
		}
	}

	expected, unknown, err := archive.ExpectedArchives(cfg, vars)
	if err != nil {
		return nil, err
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("archive names are only known after the build: %s", strings.Join(unknown, "; "))
	}
	seen := map[string]bool{}
	for _, e := range expected {
		if !seen[e.Name] {
			seen[e.Name] = true
			pl.Archives = append(pl.Archives, e.Name)
		}
	}
	if len(pl.Archives) == 0 {
		return nil, errors.New("the configuration produces no archives to release")
	}
	sort.Strings(pl.Archives)
	if !cfg.Checksum.Disable {
		name, err := tmpl.Render(cfg.Checksum.Name, vars)
		if err != nil {
			return nil, fmt.Errorf("checksum.name: %w", err)
		}
		pl.Checksum = name
	}

	for i := range cfg.Brews {
		b := &cfg.Brews[i]
		pub, err := homebrew.ResolvePublish(b, vars)
		if err != nil {
			return nil, fmt.Errorf("brews[%d] (%s): %w", i, b.Name, err)
		}
		tp := TapPlan{Formula: b.Name, Publish: pub,
			URLPrefix:   "https://github.com/" + p.Repo.String() + "/releases/download/" + v.Tag + "/",
			Completions: homebrew.CompletionLines(b)}
		if b.URLTemplate != "" {
			tp.URLPrefix = "" // checked against the rendered formula instead
		}
		pl.Taps = append(pl.Taps, tp)
	}
	return pl, nil
}

// Assets are every asset the release must hold, sorted.
func (pl *Plan) Assets() []string {
	out := slices.Clone(pl.Archives)
	if pl.Checksum != "" {
		out = append(out, pl.Checksum)
	}
	sort.Strings(out)
	return out
}

// TapSkipped reports whether no formula is published for this version.
func (pl *Plan) TapSkipped() bool {
	for _, t := range pl.Taps {
		if !t.Publish.Skip {
			return false
		}
	}
	return true
}
