// Package version derives template variables from a release tag.
package version

import (
	"fmt"
	"regexp"
	"strings"
)

// Info describes the version being released.
type Info struct {
	Tag        string // as given, e.g. "v1.2.3-rc.1"
	Version    string // tag without a leading "v"
	Major      string
	Minor      string
	Patch      string
	Prerelease string // "rc.1"
}

var (
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	semverRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)
)

// Parse validates a tag. Tags need not be semantic versions, but when they
// are, Major/Minor/Patch/Prerelease are filled.
func Parse(tag string) (Info, error) {
	if !tagRe.MatchString(tag) || strings.Contains(tag, "..") {
		return Info{}, fmt.Errorf("tag %q must be 1-128 characters of letters, digits, '.', '_', '+' and '-'", tag)
	}
	info := Info{Tag: tag, Version: strings.TrimPrefix(tag, "v")}
	if m := semverRe.FindStringSubmatch(info.Version); m != nil {
		info.Major, info.Minor, info.Patch, info.Prerelease = m[1], m[2], m[3], m[4]
	}
	return info, nil
}

// IsPrerelease reports whether the tag is a semantic-version prerelease.
func (i Info) IsPrerelease() bool { return i.Prerelease != "" }
