package github

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	scpRemoteRe = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9.-]+):([^/].*)$`)
	repoPathRe  = regexp.MustCompile(`^/*([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/*$`)
)

// ParseRemoteURL returns the host and owner/name of a git remote URL in any
// form git accepts: https://host/o/r(.git), ssh://git@host[:22]/o/r(.git),
// git@host:o/r(.git).
func ParseRemoteURL(raw string) (host, owner, name string, err error) {
	var p string
	if u, perr := url.Parse(raw); perr == nil && u.Scheme != "" && u.Host != "" {
		switch u.Scheme {
		case "https", "ssh", "git+ssh", "ssh+git":
		default:
			return "", "", "", fmt.Errorf("remote %q: unsupported scheme %s", raw, u.Scheme)
		}
		host, p = u.Hostname(), u.Path
	} else if m := scpRemoteRe.FindStringSubmatch(raw); m != nil {
		host, p = m[1], m[2]
	} else {
		return "", "", "", fmt.Errorf("remote %q is not a GitHub URL", raw)
	}
	m := repoPathRe.FindStringSubmatch(p)
	if m == nil {
		return "", "", "", fmt.Errorf("remote %q does not name owner/repository", raw)
	}
	return strings.ToLower(host), m[1], m[2], nil
}
