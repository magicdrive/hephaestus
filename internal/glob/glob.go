// Package glob implements the path patterns used by Hephaestus configuration.
//
// Syntax (slash-separated, matched segment by segment):
//
//   - any run of characters within one segment
//     ?        any single character within one segment
//     [abc]    character class, as in path.Match ([!abc] / [^abc] negate)
//     **       as a whole segment: zero or more segments
//     {a,b}    alternation; expanded before matching, not nestable
//
// Backslash escapes are not supported (backslashes are rejected outright); use
// a class such as "[*]" to match a literal metacharacter.
//
// Dot files: unless Options.Dot is set, a wildcard never matches a segment
// that begins with ".", and "**" never descends into such a directory. A
// pattern segment that itself begins with "." (".github", ".*rc") matches
// dot files as usual. This mirrors shell behavior and keeps ".git", ".env"
// and similar files out of archives unless they are asked for by name.
package glob

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// MaxAlternatives bounds brace expansion so a hostile pattern such as
// "{a,b}{a,b}{a,b}..." cannot explode.
const MaxAlternatives = 64

// MaxDoubleStars bounds the number of "**" segments in one pattern.
const MaxDoubleStars = 4

// Options adjust matching.
type Options struct {
	// Dot lets wildcards match segments beginning with ".".
	Dot bool
}

// Pattern is a compiled glob.
type Pattern struct {
	raw  string
	alts [][]string // brace-expanded alternatives, split into segments
	opts Options
}

// HasMeta reports whether s contains glob metacharacters.
func HasMeta(s string) bool {
	return strings.ContainsAny(s, "*?[{")
}

// Compile parses and validates a pattern.
func Compile(pattern string, opts Options) (*Pattern, error) {
	if pattern == "" {
		return nil, errors.New("empty pattern")
	}
	if strings.ContainsRune(pattern, '\\') {
		return nil, errors.New(`pattern must use "/" as separator; backslash escapes are not supported`)
	}
	if strings.HasPrefix(pattern, "/") {
		return nil, errors.New("pattern must be relative, not absolute")
	}
	if strings.ContainsRune(pattern, 0) {
		return nil, errors.New("pattern contains NUL")
	}
	expanded, err := expandBraces(pattern)
	if err != nil {
		return nil, err
	}
	p := &Pattern{raw: pattern, opts: opts}
	for _, alt := range expanded {
		// path.Match spells class negation "[^...]"; accept the shell's "[!...]".
		alt = strings.ReplaceAll(alt, "[!", "[^")
		segs := strings.Split(alt, "/")
		stars := 0
		for _, s := range segs {
			switch {
			case s == "":
				return nil, fmt.Errorf("pattern %q contains an empty segment or trailing slash", pattern)
			case s == "." || s == "..":
				return nil, fmt.Errorf("pattern %q contains a %q segment", pattern, s)
			case s == "**":
				stars++
			case strings.Contains(s, "**"):
				return nil, fmt.Errorf(`pattern %q: "**" must be a whole path segment`, pattern)
			default:
				// path.Match validates class syntax lazily; force it now.
				if _, err := path.Match(s, ""); err != nil {
					return nil, fmt.Errorf("pattern %q: malformed segment %q", pattern, s)
				}
			}
		}
		if stars > MaxDoubleStars {
			return nil, fmt.Errorf("pattern %q uses more than %d \"**\" segments", pattern, MaxDoubleStars)
		}
		p.alts = append(p.alts, segs)
	}
	return p, nil
}

// MustCompile is Compile that panics; for tests and constants only.
func MustCompile(pattern string, opts Options) *Pattern {
	p, err := Compile(pattern, opts)
	if err != nil {
		panic(err)
	}
	return p
}

// String returns the source pattern.
func (p *Pattern) String() string { return p.raw }

// Match reports whether the slash-separated relative path name matches.
func (p *Pattern) Match(name string) bool {
	segs := strings.Split(name, "/")
	for _, alt := range p.alts {
		if matchSegs(alt, segs, p.opts, false) {
			return true
		}
	}
	return false
}

// CouldMatchUnder reports whether some path strictly below directory dir
// could match. Walkers use it to prune directories.
func (p *Pattern) CouldMatchUnder(dir string) bool {
	if dir == "" {
		return true
	}
	segs := strings.Split(dir, "/")
	for _, alt := range p.alts {
		if matchSegs(alt, segs, p.opts, true) {
			return true
		}
	}
	return false
}

// Base returns the longest leading directory shared by every alternative that
// contains no metacharacters. Matches are reported relative to the project
// root, and placement in archives preserves structure relative to Base.
//
// For "docs/**/*.md" Base is "docs"; for "dist/*/mycli" it is "dist"; for
// "{a,b}/x" it is "".
func (p *Pattern) Base() string {
	var common []string
	for i, alt := range p.alts {
		var lit []string
		for _, s := range alt[:len(alt)-1] { // the last segment names files
			if HasMeta(s) {
				break
			}
			lit = append(lit, s)
		}
		if i == 0 {
			common = lit
			continue
		}
		n := 0
		for n < len(common) && n < len(lit) && common[n] == lit[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, "/")
}

// matchSegs matches pattern segments against name segments. With prefix set,
// it instead reports whether name could be a proper directory prefix of a
// match. Dynamic programming keeps "**" linear rather than exponential.
func matchSegs(pat, name []string, opts Options, prefix bool) bool {
	// ok[i][j]: pat[i:] can match name[j:].
	np, nn := len(pat), len(name)
	ok := make([][]bool, np+1)
	for i := range ok {
		ok[i] = make([]bool, nn+1)
	}
	ok[np][nn] = true
	for i := np - 1; i >= 0; i-- {
		ok[i][nn] = pat[i] == "**" && ok[i+1][nn]
	}
	if prefix {
		// Name exhausted while pattern remains: a deeper path may match, as
		// long as the remaining pattern can consume at least one segment.
		for i := 0; i < np; i++ {
			ok[i][nn] = true
		}
		ok[np][nn] = false
	}
	for i := np - 1; i >= 0; i-- {
		for j := nn - 1; j >= 0; j-- {
			if pat[i] == "**" {
				// Zero segments, or consume name[j] (unless hidden) and stay.
				ok[i][j] = ok[i+1][j] || (visible(name[j], opts) && ok[i][j+1])
				continue
			}
			ok[i][j] = matchSeg(pat[i], name[j], opts) && ok[i+1][j+1]
		}
	}
	return ok[0][0]
}

func visible(seg string, opts Options) bool {
	return opts.Dot || !strings.HasPrefix(seg, ".")
}

func matchSeg(pat, seg string, opts Options) bool {
	if strings.HasPrefix(seg, ".") && !opts.Dot && !strings.HasPrefix(pat, ".") {
		return false
	}
	ok, err := path.Match(pat, seg)
	return err == nil && ok
}

// expandBraces expands non-nested {a,b,c} groups.
func expandBraces(p string) ([]string, error) {
	out := []string{""}
	for len(p) > 0 {
		open := strings.IndexByte(p, '{')
		if open < 0 {
			if strings.IndexByte(p, '}') >= 0 {
				return nil, errors.New("unbalanced '}' in pattern")
			}
			for i := range out {
				out[i] += p
			}
			break
		}
		lit := p[:open]
		if strings.IndexByte(lit, '}') >= 0 {
			return nil, errors.New("unbalanced '}' in pattern")
		}
		rest := p[open+1:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return nil, errors.New("unbalanced '{' in pattern")
		}
		body := rest[:end]
		if strings.IndexByte(body, '{') >= 0 {
			return nil, errors.New("nested braces are not supported")
		}
		choices := strings.Split(body, ",")
		next := make([]string, 0, len(out)*len(choices))
		for _, o := range out {
			for _, c := range choices {
				next = append(next, o+lit+c)
			}
		}
		if len(next) > MaxAlternatives {
			return nil, fmt.Errorf("pattern expands to more than %d alternatives", MaxAlternatives)
		}
		out = next
		p = rest[end+1:]
	}
	return out, nil
}
