package publish

import (
	"errors"
	"fmt"
	"strings"

	"github.com/magicdrive/hephaestus/internal/version"
)

// Version is a release version accepted by publish.
type Version struct {
	Tag        string // "v1.2.3-rc.1"
	Version    string // "1.2.3-rc.1"
	Prerelease bool
	Info       version.Info
}

// ParseVersion validates a release tag. Release tags are SemVer 2.0.0 with
// a leading "v" and no build metadata. Input without the "v" is rejected,
// not normalized: the tag is pushed and cannot be taken back, so it must be
// written exactly as it will be published.
func ParseVersion(s string) (Version, error) {
	if s == "" {
		return Version{}, errors.New("a version is required, e.g. v1.2.3 or v1.2.3-rc.1")
	}
	if !strings.HasPrefix(s, "v") {
		if info, err := version.Parse("v" + s); err == nil && info.IsSemver() {
			return Version{}, fmt.Errorf("version %q must start with \"v\": did you mean v%s?", s, s)
		}
		return Version{}, fmt.Errorf("version %q is not a release tag like v1.2.3 or v1.2.3-rc.1", s)
	}
	info, err := version.Parse(s)
	if err != nil {
		return Version{}, err
	}
	if !info.IsSemver() {
		return Version{}, fmt.Errorf("version %q is not SemVer: want vMAJOR.MINOR.PATCH[-PRERELEASE], e.g. v1.2.3 or v1.2.3-rc.1", s)
	}
	if info.Build != "" {
		return Version{}, fmt.Errorf("version %q has build metadata (+%s): release tags do not carry build metadata", s, info.Build)
	}
	for _, id := range strings.Split(info.Prerelease, ".") {
		if len(id) > 1 && id[0] == '0' && strings.Trim(id, "0123456789") == "" {
			return Version{}, fmt.Errorf("version %q: numeric pre-release identifier %q has a leading zero (SemVer 2.0.0 §9)", s, id)
		}
	}
	return Version{Tag: info.Tag, Version: info.Version, Prerelease: info.IsPrerelease(), Info: info}, nil
}

// Kind is "prerelease" or "stable".
func (v Version) Kind() string {
	if v.Prerelease {
		return "prerelease"
	}
	return "stable"
}

// Args are the ways a version reaches publish: positional arguments (make
// publish v1.2.3), VERSION=v1.2.3 and tag=v1.2.3 (the form the sibling
// projects use) on the make command line.
type Args struct {
	Positional []string
	VersionVar string
	TagVar     string
}

// ResolveArgs returns the one version the arguments name. Exactly one
// source with exactly one value is accepted; anything else is an error,
// even when the values agree, so a typo cannot pick the wrong one.
func ResolveArgs(a Args) (string, error) {
	var sources []string
	if len(a.Positional) > 1 {
		return "", fmt.Errorf("one version is expected, got %d: %s", len(a.Positional), strings.Join(a.Positional, " "))
	}
	if len(a.Positional) == 1 {
		sources = append(sources, a.Positional[0])
	}
	if a.VersionVar != "" {
		sources = append(sources, "VERSION="+a.VersionVar)
	}
	if a.TagVar != "" {
		sources = append(sources, "tag="+a.TagVar)
	}
	switch len(sources) {
	case 0:
		return "", errors.New("a version is required: make publish v1.2.3 (or VERSION=v1.2.3)")
	case 1:
	default:
		return "", fmt.Errorf("give the version once, got %s", strings.Join(sources, " and "))
	}
	switch {
	case len(a.Positional) == 1:
		return a.Positional[0], nil
	case a.VersionVar != "":
		return a.VersionVar, nil
	default:
		return a.TagVar, nil
	}
}
