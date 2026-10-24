package archive

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Expected is an archive a release of the configuration will contain,
// derived before anything is built.
type Expected struct {
	ID     string // archives[].id, or the importing build's id
	Name   string // file name, e.g. tool_1.2.3_linux_amd64.tar.gz
	Target config.Target
	// Imported is true for archives taken from a build (existing kind
	// archive) rather than written by the archive stage.
	Imported bool
}

// ExpectedArchives lists the archives the configuration produces for the
// version in vars. Targets come from archives[].targets or from the builds
// an archive packages, as the archive stage resolves them after the build.
// Archives whose names are only known after the build (GoReleaser imports)
// are reported in unknown instead.
func ExpectedArchives(cfg *config.Config, vars tmpl.Vars) (out []Expected, unknown []string, err error) {
	for i := range cfg.Archives {
		a := &cfg.Archives[i]
		for _, t := range expectedTargets(cfg, a) {
			format := a.Format
			for _, o := range a.FormatOverrides {
				if o.OS == t.OS {
					format = o.Format
				}
			}
			base, err := tmpl.Render(a.Name, vars.WithTarget(t.OS, t.Arch, t.Arm))
			if err != nil {
				return nil, nil, fmt.Errorf("archives[%d].name: %w", i, err)
			}
			out = append(out, Expected{ID: a.ID, Name: base + "." + format, Target: t})
		}
	}
	for i := range cfg.Builds {
		b := &cfg.Builds[i]
		switch {
		case b.Existing != nil && b.Existing.Kind == config.ExistingKindArchive:
			for _, t := range config.ExpandTargets(b.Targets, b.Matrix) {
				p, err := tmpl.Render(b.Existing.Path, vars.WithTarget(t.OS, t.Arch, t.Arm))
				if err != nil {
					return nil, nil, fmt.Errorf("builds[%d].existing.path: %w", i, err)
				}
				out = append(out, Expected{ID: b.ID, Name: path.Base(p), Target: t, Imported: true})
			}
		case b.GoReleaser != nil && b.GoReleaser.Archives:
			unknown = append(unknown, fmt.Sprintf("builds[%d] (id %s) imports GoReleaser archives, named by GoReleaser", i, b.ID))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, unknown, nil
}

// expectedTargets are an archive's targets: its own, or those of the builds
// it packages.
func expectedTargets(cfg *config.Config, a *config.Archive) []config.Target {
	if len(a.Targets) > 0 {
		return a.Targets
	}
	var out []config.Target
	seen := map[string]bool{}
	for _, b := range cfg.Builds {
		if b.ImportsArchives() || len(a.Builds) > 0 && !slices.Contains(a.Builds, b.ID) {
			continue
		}
		for _, t := range config.ExpandTargets(b.Targets, b.Matrix) {
			if !seen[t.String()] {
				seen[t.String()] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// AssetNameProblem reports why name cannot be a release asset name as is
// (GitHub renames other characters), or nil.
func AssetNameProblem(name string) error {
	if !assetNameRe.MatchString(name) || len(name) > 255 {
		return fmt.Errorf("%q may only contain letters, digits, '.', '_' and '-' (GitHub would rename other characters)", name)
	}
	return nil
}

// FoldCollisions returns the groups of names that differ only in case:
// GitHub treats asset names case-insensitively.
func FoldCollisions(names []string) [][]string {
	groups := map[string][]string{}
	var keys []string
	for _, n := range names {
		k := strings.ToLower(n)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], n)
	}
	var out [][]string
	for _, k := range keys {
		if len(groups[k]) > 1 {
			out = append(out, groups[k])
		}
	}
	return out
}
