// Package pipeline decides which stages run and verifies, before any of them
// starts, that each one will have the inputs it needs.
package pipeline

import (
	"errors"
	"fmt"
	"strings"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

// Stage names a pipeline stage.
type Stage string

// Stages in execution order.
const (
	Build    Stage = "build"
	Archive  Stage = "archive"
	Checksum Stage = "checksum"
	Release  Stage = "release"
	Formula  Stage = "formula"
	Tap      Stage = "tap"
)

// Order is the canonical execution order.
var Order = []Stage{Build, Archive, Checksum, Release, Formula, Tap}

// ParseStages parses a comma-separated list ("all" for every stage). The
// result is in canonical order regardless of input order, because stages
// feed each other through the manifest.
func ParseStages(s string) ([]Stage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no stages given")
	}
	want := map[Stage]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "all" {
			for _, st := range Order {
				want[st] = true
			}
			continue
		}
		st := Stage(part)
		if !st.valid() {
			return nil, fmt.Errorf("unknown stage %q (stages: %s, or all)", part, joinStages(Order))
		}
		if want[st] {
			return nil, fmt.Errorf("stage %q listed twice", part)
		}
		want[st] = true
	}
	var out []Stage
	for _, st := range Order {
		if want[st] {
			out = append(out, st)
		}
	}
	return out, nil
}

func (s Stage) valid() bool {
	for _, st := range Order {
		if st == s {
			return true
		}
	}
	return false
}

func joinStages(st []Stage) string {
	parts := make([]string, len(st))
	for i, s := range st {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// Facts describe what is available before the run starts.
type Facts struct {
	Manifest *manifest.Manifest // loaded from disk; may be empty

	HasBuilds        bool
	BuildsArchives   bool // some build imports prebuilt archives
	HasArchives      bool
	ArchivesSelfFed  bool // every archive declares targets and files
	ChecksumDisabled bool
	HasGitHubRelease bool
	HasBrews         bool
	BrewsHaveURL     bool // every brew has url_template
	BrewsHaveTap     bool // every brew names repository owner and name

	Tag       string
	Repo      string // owner/name for releases
	Token     bool
	TapToken  bool
	DryRun    bool
	SkipTapIf bool // every brew skips the tap (skip_upload: true, or auto on a prerelease)
}

// Check returns every missing input for the requested stages, considering
// what earlier stages in the same run will produce.
func Check(stages []Stage, f Facts) error {
	run := map[Stage]bool{}
	for _, s := range stages {
		run[s] = true
	}
	have := func(k manifest.Kind) bool {
		return f.Manifest != nil && len(f.Manifest.Filter(k)) > 0
	}
	var errs []error
	miss := func(s Stage, format string, args ...any) {
		errs = append(errs, fmt.Errorf("stage %s: %s", s, fmt.Sprintf(format, args...)))
	}
	binaries := have(manifest.KindBinary) || run[Build]
	archives := have(manifest.KindArchive) || run[Archive] || (run[Build] && f.BuildsArchives)
	checksums := have(manifest.KindChecksum) || (run[Checksum] && !f.ChecksumDisabled)
	formulas := have(manifest.KindFormula) || run[Formula]
	urls := run[Release]
	if f.Manifest != nil {
		for _, a := range f.Manifest.Filter(manifest.KindArchive) {
			if a.DownloadURL != "" {
				urls = true
			}
		}
	}

	for _, s := range stages {
		switch s {
		case Build:
			if !f.HasBuilds {
				miss(s, "no builds are configured")
			}
		case Archive:
			if !f.HasArchives {
				miss(s, "no archives are configured")
			} else if !binaries && !f.ArchivesSelfFed {
				miss(s, "no binaries: run the build stage, import artifacts with a goreleaser/existing build, or give every archive explicit targets and files")
			}
		case Checksum:
			if f.ChecksumDisabled {
				miss(s, "checksum.disable is true")
			}
			if !archives {
				miss(s, "no archives: run the archive stage or import archives")
			}
		case Release:
			if !f.HasGitHubRelease {
				miss(s, "release.github is not configured or not enabled")
			}
			if f.Tag == "" {
				miss(s, "a tag is required (--tag, or run on a tag push)")
			}
			if f.Repo == "" {
				miss(s, "the repository is unknown (set release.github.owner/name or GITHUB_REPOSITORY)")
			}
			if !f.Token && !f.DryRun {
				miss(s, "a GitHub token is required (GITHUB_TOKEN or --token-env)")
			}
			if !archives && !checksums {
				miss(s, "nothing to upload: no archives or checksums")
			}
		case Formula:
			if !f.HasBrews {
				miss(s, "no brews (homebrew) are configured")
			}
			if !archives {
				miss(s, "no archives: the formula needs archive checksums")
			}
			if f.Tag == "" {
				miss(s, "a tag is required (--tag, or run on a tag push)")
			}
			if !urls && !f.BrewsHaveURL && f.Repo == "" {
				miss(s, "download URLs are unknown: run the release stage, set url_template, or set the repository")
			}
		case Tap:
			if !formulas {
				miss(s, "no formulas: run the formula stage first")
			}
			if !f.BrewsHaveTap {
				miss(s, "every brew needs repository.owner and repository.name")
			}
			if !f.TapToken && !f.DryRun && !f.SkipTapIf {
				miss(s, "a token with write access to the tap is required (HOMEBREW_TAP_GITHUB_TOKEN or repository.token); GITHUB_TOKEN cannot write to another repository")
			}
		}
	}
	return errors.Join(errs...)
}
