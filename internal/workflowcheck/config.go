package workflowcheck

import (
	"github.com/magicdrive/hephaestus/internal/archive"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/tmpl"
	"github.com/magicdrive/hephaestus/internal/version"
)

// FromConfig fills what Options takes from the configuration: the
// preflight settings, whether a formula goes to a tap, and the release
// asset names of the version in vars.
func FromConfig(o Options, cfg *config.Config, vars tmpl.Vars) Options {
	if o.Workflow == "" {
		o.Workflow = cfg.Preflight.Workflow
	}
	if o.Environment == "" {
		o.Environment = cfg.Preflight.Environment
	}
	if o.TapTokenSecret == "" {
		o.TapTokenSecret = cfg.Preflight.TapTokenSecret
	}
	for _, b := range cfg.Brews {
		if b.Repository.Owner != "" && b.SkipUpload != "true" {
			o.Formulae = true
		}
	}
	o.Assets, o.Unknown, o.AssetsErr = PlannedAssets(cfg, vars)
	return o
}

// PlannedAssets returns the release asset names of the version in vars:
// every archive, then the checksum file. Archives named only after
// building are listed in unknown.
func PlannedAssets(cfg *config.Config, vars tmpl.Vars) (names, unknown []string, err error) {
	expected, unknown, err := archive.ExpectedArchives(cfg, vars)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range expected {
		names = append(names, e.Name)
	}
	if !cfg.Checksum.Disable && len(cfg.Archives)+len(expected) > 0 {
		cs, err := tmpl.Render(cfg.Checksum.Name, vars)
		if err != nil {
			return nil, nil, err
		}
		names = append(names, cs)
	}
	return names, unknown, nil
}

// TagVars are the template variables of tag for project names (no commit
// and no environment: release asset names do not depend on them).
func TagVars(cfg *config.Config, tag string) (tmpl.Vars, error) {
	info, err := version.Parse(tag)
	if err != nil {
		return tmpl.Vars{}, err
	}
	n := cfg.Project.Name
	return tmpl.Vars{Project: n, ProjectName: n, Version: info.Version, Tag: info.Tag, RawVersion: info.Tag,
		Major: info.Major, Minor: info.Minor, Patch: info.Patch, Prerelease: info.Prerelease, Env: map[string]string{}}, nil
}
