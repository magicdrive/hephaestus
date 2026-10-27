package main

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/tap"
)

// -strict: the checks a required status check needs to fail closed.
//
// Only the formulae Homebrew loads from the tap are checked, as Homebrew
// finds them: every *.rb under Formula/ (sharded subdirectories included)
// when the tap has that directory, else under HomebrewFormula/, else the
// *.rb files at the root of the tap (not in subdirectories: cmd/, Casks/,
// lib/ hold other Ruby). A Formula or HomebrewFormula entry that is not a
// directory (a symlink, a file) fails: Homebrew would follow it to
// formulae the guard does not see. Every other changed *.rb is listed as
// skipped.
//
// Contents are read from git objects (ls-tree, cat-file), never from the
// work tree, so a path cannot reach outside the repository; paths that are
// not clean relative paths (.., empty components, control characters) and
// formulae that are not regular files (symlinks, submodules) fail.
//
// A finding is either about which version and artifacts the tap serves
// (overridable: -allow-downgrade reports it as a warning) or about a
// formula the guard cannot read (never overridable).

// overridable findings: an intentional rollback, a removal or a re-release
// is a decision someone can take; -allow-downgrade (kept for
// compatibility) reports them as warnings. An authorized rollback
// (approval.go) lowers only the one downgrade it names.
const overridable = true

// heredocStartRe matches the start of a heredoc (<<~EOS, <<-EOS, <<EOS,
// quoted or not), as internal/tap's formula parser does.
var heredocStartRe = regexp.MustCompile(`<<[~-]?(["']?)([A-Za-z_][A-Za-z0-9_]*)(["']?)`)

var urlStmtRe = regexp.MustCompile(`^url[ \t(]+["']([^"'\n]+)["']`)

// artifactKeywords start the statements that select what is downloaded
// and installed (besides version, which is compared on its own).
var artifactKeywords = map[string]bool{
	"url": true, "mirror": true, "sha256": true, "head": true, "root_url": true,
	"patch": true, "resource": true,
}

// formulaURLBlocks may enclose a url of the formula itself (not of a
// resource, a patch or head).
var formulaURLBlocks = map[string]bool{
	"class": true, "on_macos": true, "on_linux": true, "on_arm": true, "on_intel": true,
	"if": true, "elsif": true, "else": true, "unless": true, "stable": true,
}

type treeEntry struct {
	exists  bool
	regular bool
	mode    string
	content []byte
}

func (g gitRepo) checkStrict(from, head string, files []string, opt options, w io.Writer) (bool, error) {
	allow := opt.allow
	oldDir, oldBad, err := g.formulaDir(from)
	if err != nil {
		return false, err
	}
	newDir, newBad, err := g.formulaDir(head)
	if err != nil {
		return false, err
	}
	failed := false
	// report prints a finding: an error, or a warning when it is
	// overridable and -allow-downgrade is set.
	report := func(file string, canOverride bool, finding, advice string) {
		if canOverride && allow {
			annotate(w, "warning", file, finding+" (allowed: -allow-downgrade)")
			return
		}
		if advice != "" {
			finding += ". " + advice
		}
		annotate(w, "error", file, finding)
		failed = true
	}
	if newBad != "" {
		report(newBad, false, newBad+" is not a directory: Homebrew would load the formulae it points to, which the guard does not check", "Make it a directory")
	}
	if oldBad != "" {
		report(oldBad, overridable, oldBad+" was not a directory before this change: the formulae cannot be compared", "")
	}
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	if oldDir != newDir {
		// The tap's layout changed: every formula of either side counts
		// (those of the old layout are removed by it).
		for _, rev := range []string{from, head} {
			all, err := g.rubyFiles(rev)
			if err != nil {
				return false, err
			}
			for _, f := range all {
				set[f] = true
			}
		}
	}
	paths := make([]string, 0, len(set))
	for f := range set {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if why := unsafePath(p); why != "" {
			report(p, false, fmt.Sprintf("%q: %s", p, why), "Rename it")
			continue
		}
		inOld, inNew := isFormulaPath(oldDir, p), isFormulaPath(newDir, p)
		if !inOld && !inNew {
			fmt.Fprintf(w, "skip %s: not a formula (Homebrew loads %s)\n", escapeData(p), layoutName(newDir))
			continue
		}
		var o, n treeEntry
		if inOld {
			if o, err = g.entry(from, p); err != nil {
				return false, err
			}
		}
		if inNew {
			if n, err = g.entry(head, p); err != nil {
				return false, err
			}
		}
		judge(p, o, n, report, opt.rollbacks, w)
	}
	return failed, nil
}

// judge reports the findings of one formula, o before and n after. A
// downgrade that rb authorizes exactly is a warning; nothing else is.
func judge(p string, o, n treeEntry, report func(string, bool, string, string), rb *rollbacks, w io.Writer) {
	switch {
	case !o.exists && !n.exists:
		return
	case n.exists && !n.regular:
		report(p, false, fmt.Sprintf("%s: not a regular file (mode %s): Homebrew would load what it points to, which the guard cannot check", p, n.mode), "Commit the formula itself")
		return
	case !n.exists:
		v := "(not a regular file)"
		if o.regular {
			v = orNone(tap.FormulaVersion(o.content))
		}
		report(p, overridable, fmt.Sprintf("%s: formula removed (was %s)", p, v), "A rollback approval never covers a removal; removing a formula is the owner's decision outside this check")
		return
	}
	newV := tap.FormulaVersion(n.content)
	if newV == "" {
		report(p, false, p+": no version (no version statement in the class body and no semantic version in its url): a malformed formula", "Fix the formula")
		return
	}
	if !isSemver(newV) {
		report(p, overridable, fmt.Sprintf("%s: version %q is not a semantic version: its order cannot be checked", p, newV), "")
		return
	}
	findings := 0
	for _, u := range urlVersionMismatches(n.content, newV) {
		report(p, overridable, fmt.Sprintf("%s: url %s is of version %s, the formula's version is %s", p, u.url, u.version, newV), "Every platform's url must be of the formula's version")
		findings++
	}
	if !o.exists {
		if findings == 0 {
			fmt.Fprintf(w, "ok %s: added at %s\n", escapeData(p), escapeData(newV))
		}
		return
	}
	if !o.regular {
		report(p, overridable, fmt.Sprintf("%s: was not a regular file (mode %s): the versions cannot be compared", p, o.mode), "")
		return
	}
	oldV := tap.FormulaVersion(o.content)
	c, ok := tap.CompareVersions(oldV, newV)
	switch {
	case !ok:
		report(p, overridable, fmt.Sprintf("%s: cannot compare version %s with %s (not semantic versions)", p, orNone(oldV), newV), "Check the order by hand")
	case c > 0:
		if e := rb.match(p, oldV, newV); e != nil {
			e.used = true
			annotate(w, "warning", p, fmt.Sprintf("%s: version goes down from %s to %s (authorized rollback: %s)", p, oldV, newV, e.describe()))
			return
		}
		report(p, overridable, fmt.Sprintf("%s: version goes down from %s to %s", p, oldV, newV), downgradeAdvice)
	case c == 0:
		if what := artifactChange(o.content, n.content); what != "" {
			report(p, overridable, fmt.Sprintf("%s: version %s is unchanged but its artifacts changed (%s): a re-release under the same version cannot be told from a swapped artifact", p, newV, what), "Release a new version")
			return
		}
		if findings == 0 {
			fmt.Fprintf(w, "ok %s: %s -> %s\n", escapeData(p), escapeData(oldV), escapeData(newV))
		}
	default:
		if findings == 0 {
			fmt.Fprintf(w, "ok %s: %s -> %s\n", escapeData(p), escapeData(oldV), escapeData(newV))
		}
	}
}

func isSemver(v string) bool {
	_, ok := tap.CompareVersions(v, v)
	return ok
}

// unsafePath says why p is not a clean path relative to the tap's root
// ("" when it is).
func unsafePath(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return "not a relative path"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "control character in the path"
		}
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return "empty, . or .. path component"
		}
	}
	return ""
}

// isFormulaPath reports whether Homebrew loads p as a formula of a tap
// whose formula directory is dir ("" for the root).
func isFormulaPath(dir, p string) bool {
	if !strings.HasSuffix(p, ".rb") {
		return false
	}
	if dir == "" {
		return !strings.Contains(p, "/")
	}
	return strings.HasPrefix(p, dir+"/")
}

func layoutName(dir string) string {
	if dir == "" {
		return "*.rb at the root of the tap"
	}
	return dir + "/**/*.rb"
}

// formulaDir returns the directory Homebrew loads the formulae of the tap
// at rev from: Formula, else HomebrewFormula, else "" (the root). bad names
// that entry when it exists but is not a directory.
func (g gitRepo) formulaDir(rev string) (dir, bad string, err error) {
	out, err := g.git("--literal-pathspecs", "ls-tree", "--full-tree", "-z", rev, "--", "Formula", "HomebrewFormula")
	if err != nil {
		return "", "", err
	}
	types := map[string]string{}
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		if f := strings.Fields(meta); len(f) == 3 {
			types[name] = f[1]
		}
	}
	for _, d := range []string{"Formula", "HomebrewFormula"} {
		if t, ok := types[d]; ok {
			if t != "tree" {
				return d, d, nil
			}
			return d, "", nil
		}
	}
	return "", "", nil
}

// rubyFiles lists every *.rb file at rev.
func (g gitRepo) rubyFiles(rev string) ([]string, error) {
	out, err := g.git("ls-tree", "-r", "--full-tree", "-z", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(f, ".rb") {
			files = append(files, f)
		}
	}
	return files, nil
}

// entry reads p at rev from the tree (exists false when rev has none);
// the content only of a regular file.
func (g gitRepo) entry(rev, p string) (treeEntry, error) {
	out, err := g.git("--literal-pathspecs", "ls-tree", "--full-tree", "-z", rev, "--", p)
	if err != nil {
		return treeEntry{}, err
	}
	rec := strings.TrimSuffix(string(out), "\x00")
	if rec == "" {
		return treeEntry{}, nil
	}
	meta, name, ok := strings.Cut(rec, "\t")
	f := strings.Fields(meta)
	if !ok || name != p || len(f) != 3 {
		return treeEntry{}, fmt.Errorf("git ls-tree %s -- %q: unexpected output %q", rev, p, rec)
	}
	e := treeEntry{exists: true, mode: f[0], regular: f[1] == "blob" && (f[0] == "100644" || f[0] == "100755")}
	if e.regular {
		if e.content, err = g.git("cat-file", "blob", f[2]); err != nil {
			return treeEntry{}, err
		}
	}
	return e, nil
}

type formulaURL struct{ url, version string }

// statements are the lines of a formula that select its artifacts, each
// with the statements enclosing it (by indentation, the layout of
// generated and brew-style formulae), and the urls of the formula itself.
// Heredocs (caveats) are skipped; the data after __END__ (patch :DATA)
// counts.
func statements(content []byte) (artifacts []string, urls []string) {
	type frame struct {
		indent int
		text   string
	}
	var stack []frame
	heredoc := ""
	lines := strings.Split(string(content), "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimLeft(line, " \t")
		if heredoc != "" {
			if trimmed == heredoc {
				heredoc = ""
			}
			continue
		}
		if line == "__END__" {
			for _, d := range lines[i+1:] {
				artifacts = append(artifacts, "__END__ "+d)
			}
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := heredocStartRe.FindStringSubmatch(trimmed); m != nil && m[1] == m[3] {
			heredoc = m[2]
		}
		indent := len(line) - len(trimmed)
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		norm := strings.Join(strings.Fields(trimmed), " ")
		if kw := keyword(trimmed); artifactKeywords[kw] {
			ctx := make([]string, 0, len(stack)+1)
			own := true
			for _, f := range stack {
				ctx = append(ctx, f.text)
				own = own && formulaURLBlocks[keyword(f.text)]
			}
			artifacts = append(artifacts, strings.Join(append(ctx, norm), " > "))
			if m := urlStmtRe.FindStringSubmatch(trimmed); m != nil && kw == "url" && own {
				urls = append(urls, m[1])
			}
		}
		stack = append(stack, frame{indent, norm})
	}
	return artifacts, urls
}

// keyword is the first word of a statement.
func keyword(s string) string {
	if i := strings.IndexAny(s, " \t(:"); i >= 0 {
		return s[:i]
	}
	return s
}

// artifactChange names the kinds of artifact statements that differ
// between a and b ("" when none does).
func artifactChange(a, b []byte) string {
	x, _ := statements(a)
	y, _ := statements(b)
	if slices.Equal(x, y) {
		return ""
	}
	count := map[string]int{}
	for _, s := range x {
		count[s]++
	}
	for _, s := range y {
		count[s]--
	}
	seen := map[string]bool{}
	var kinds []string
	for _, s := range append(x, y...) {
		if count[s] == 0 {
			continue
		}
		parts := strings.Split(s, " > ")
		k := keyword(parts[len(parts)-1])
		if strings.HasPrefix(s, "__END__ ") {
			k = "__END__ data"
		}
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return "their order"
	}
	return strings.Join(kinds, ", ")
}

// urlVersionMismatches lists the urls of the formula itself whose path
// names a semantic version other than v (an older artifact for one
// platform).
func urlVersionMismatches(content []byte, v string) []formulaURL {
	_, urls := statements(content)
	var bad []formulaURL
	for _, s := range urls {
		u, err := url.Parse(s)
		if err != nil {
			continue
		}
		for _, seg := range strings.Split(u.Path, "/") {
			if !isSemver(seg) {
				continue
			}
			if c, _ := tap.CompareVersions(seg, v); c != 0 {
				bad = append(bad, formulaURL{s, strings.TrimPrefix(seg, "v")})
			}
			break
		}
	}
	return bad
}
