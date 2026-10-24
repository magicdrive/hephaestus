package homebrew

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

// installReceivers are the Formula path helpers whose install calls are
// checked. Paths are relative to the directory Homebrew runs install in.
var installReceivers = map[string]bool{
	"bin": true, "sbin": true, "lib": true, "libexec": true, "include": true,
	"prefix": true, "share": true, "pkgshare": true, "etc": true, "doc": true,
	"man": true, "man1": true, "man2": true, "man3": true, "man4": true,
	"man5": true, "man6": true, "man7": true, "man8": true,
	"bash_completion": true, "zsh_completion": true, "fish_completion": true,
	"pwsh_completion": true, "elisp": true, "frameworks": true,
}

// installCall is one statically analyzed `<receiver>.install ...` call.
type installCall struct {
	line     int
	receiver string // "" when the receiver is an expression
	srcs     []string
	dests    map[string]string // src -> installed name for "src" => "name"
	reason   string            // non-empty: cannot be verified, and why
	// platforms restricts the call to archives matching every predicate
	// (from `if OS.mac?` blocks or modifiers); nil means all archives.
	platforms []platformPred
}

// platformPred is a recognized platform condition, possibly negated.
type platformPred struct {
	name string // "OS.mac?", "OS.linux?", "Hardware::CPU.arm?", "Hardware::CPU.intel?"
	neg  bool
}

func (p platformPred) match(a manifest.Artifact) bool {
	var v bool
	switch p.name {
	case "OS.mac?":
		v = a.OS == "darwin"
	case "OS.linux?":
		v = a.OS == "linux"
	case "Hardware::CPU.arm?":
		v = a.Arch == "arm64"
	case "Hardware::CPU.intel?":
		v = a.Arch == "amd64"
	}
	return v != p.neg
}

type frame struct {
	known bool // a recognized if/unless platform condition
	preds []platformPred
}

// analyzeInstall extracts install calls from Ruby install code.
func analyzeInstall(code string) []installCall {
	toks := lexRuby(code)
	var calls []installCall
	var stack []frame
	stmtStart := true
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tkNL {
			stmtStart = true
			continue
		}
		atStart := stmtStart
		stmtStart = false
		if t.kind == tkIdent {
			switch t.text {
			case "if", "unless":
				if atStart {
					preds, ok, next := parseCond(toks, i+1)
					if t.text == "unless" {
						preds, ok = negate(preds, ok)
					}
					stack = append(stack, frame{known: ok, preds: preds})
					i = next - 1
					continue
				}
			case "else":
				if n := len(stack); n > 0 {
					stack[n-1].preds, stack[n-1].known = negate(stack[n-1].preds, stack[n-1].known)
				}
				continue
			case "elsif":
				if n := len(stack); n > 0 {
					stack[n-1].known = false
				}
				continue
			case "while", "until", "case", "def", "class", "module", "begin":
				if atStart {
					stack = append(stack, frame{})
				}
				continue
			case "do":
				stack = append(stack, frame{})
				continue
			case "end":
				if n := len(stack); n > 0 {
					stack = stack[:n-1]
				}
				continue
			}
		}
		if !(t.kind == tkIdent && t.text == "install") || i == 0 || toks[i-1].text != "." || toks[i-1].kind != tkPunct {
			continue
		}
		call := installCall{line: t.line, dests: map[string]string{}}
		if i >= 2 && toks[i-2].kind == tkIdent && installReceivers[toks[i-2].text] &&
			(i < 3 || toks[i-3].kind == tkNL || (toks[i-3].kind == tkPunct && toks[i-3].text != "." && toks[i-3].text != "::")) {
			call.receiver = toks[i-2].text
		} else {
			call.reason = "the receiver is an expression"
		}
		next := parseInstallArgs(toks, i+1, &call)
		// Statement modifier: `bin.install "x" if OS.mac?`.
		if next < len(toks) && toks[next].kind == tkIdent && (toks[next].text == "if" || toks[next].text == "unless") {
			preds, ok, after := parseCond(toks, next+1)
			if toks[next].text == "unless" {
				preds, ok = negate(preds, ok)
			}
			if !ok && call.reason == "" {
				call.reason = "it is inside a condition that cannot be evaluated statically"
			}
			call.platforms = append(call.platforms, preds...)
			next = after
		} else if next < len(toks) && toks[next].kind != tkNL && call.reason == "" {
			call.reason = "the arguments are not literal"
		}
		for _, f := range stack {
			if !f.known {
				if call.reason == "" {
					call.reason = "it is inside a block or condition that cannot be evaluated statically"
				}
				continue
			}
			call.platforms = append(call.platforms, f.preds...)
		}
		calls = append(calls, call)
		i = next - 1
	}
	return calls
}

// parseCond reads a condition up to the end of the line: platform predicates
// joined by &&, each optionally negated with !.
func parseCond(toks []token, i int) ([]platformPred, bool, int) {
	var preds []platformPred
	ok := true
	want := true // expecting a predicate
	for ; i < len(toks) && toks[i].kind != tkNL; i++ {
		if toks[i].kind == tkIdent && toks[i].text == "then" {
			continue
		}
		if !want {
			if toks[i].kind == tkPunct && toks[i].text == "&&" {
				want = true
				continue
			}
			ok = false
			continue
		}
		neg := false
		if toks[i].kind == tkPunct && toks[i].text == "!" {
			neg = true
			i++
		}
		name, n := predicateAt(toks, i)
		if name == "" {
			ok = false
			continue
		}
		preds = append(preds, platformPred{name: name, neg: neg})
		i += n - 1
		want = false
	}
	if want {
		ok = false
	}
	return preds, ok, i
}

func predicateAt(toks []token, i int) (string, int) {
	text := func(j int) string {
		if j < len(toks) {
			return toks[j].text
		}
		return ""
	}
	if text(i) == "OS" && text(i+1) == "." && (text(i+2) == "mac?" || text(i+2) == "linux?") {
		return "OS." + text(i+2), 3
	}
	if text(i) == "Hardware" && text(i+1) == "::" && text(i+2) == "CPU" && text(i+3) == "." && (text(i+4) == "arm?" || text(i+4) == "intel?") {
		return "Hardware::CPU." + text(i+4), 5
	}
	return "", 0
}

// negate turns "if A && B" into its else branch. Only a single predicate can
// be negated exactly; otherwise the branch is unknown.
func negate(preds []platformPred, ok bool) ([]platformPred, bool) {
	if !ok || len(preds) != 1 {
		return nil, false
	}
	return []platformPred{{name: preds[0].name, neg: !preds[0].neg}}, true
}

// parseInstallArgs reads literal install arguments: strings, arrays of
// strings, %w[] arrays and "src" => "dest" pairs, optionally parenthesized.
// It returns the index of the first token after the arguments.
func parseInstallArgs(toks []token, i int, call *installCall) int {
	paren := i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "("
	if paren {
		i++
	}
	depth := 0
	expectValue := false // after "=>"
	var lastSrc []string
	sawArg := false
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == tkNL:
			// Arguments continue on the next line after "," "[" "(" "=>".
			if depth > 0 || paren || expectValue || (i > 0 && toks[i-1].kind == tkPunct && toks[i-1].text == ",") {
				continue
			}
			goto done
		case t.kind == tkString:
			sawArg = true
			if t.dyn {
				call.fail("an argument interpolates or uses escapes")
				continue
			}
			if expectValue {
				for _, s := range lastSrc {
					call.dests[s] = t.text
				}
				expectValue = false
				continue
			}
			call.srcs = append(call.srcs, t.text)
			lastSrc = []string{t.text}
			if depth > 0 {
				lastSrc = nil
			}
		case t.kind == tkWords:
			sawArg = true
			if t.dyn {
				call.fail("a word array interpolates")
				continue
			}
			call.srcs = append(call.srcs, t.words...)
			lastSrc = append([]string(nil), t.words...)
		case t.kind == tkPunct && t.text == "[":
			depth++
			lastSrc = nil
		case t.kind == tkPunct && t.text == "]":
			depth--
			if depth < 0 {
				call.fail("unbalanced brackets")
				goto done
			}
		case t.kind == tkPunct && t.text == ",":
		case t.kind == tkPunct && t.text == "=>":
			if depth > 0 || lastSrc == nil {
				call.fail("a hash key is not a literal path")
			}
			expectValue = true
		case t.kind == tkPunct && t.text == ")" && paren && depth == 0:
			i++
			goto done
		case t.kind == tkIdent && depth == 0 && !paren && (t.text == "if" || t.text == "unless"):
			goto done
		default:
			call.fail("an argument is not a string literal")
			sawArg = true
		}
	}
done:
	if !sawArg && call.reason == "" {
		call.fail("there are no arguments")
	}
	return i
}

func (c *installCall) fail(reason string) {
	if c.reason == "" {
		c.reason = reason
	}
}

// archiveTree is an archive's content as seen by `def install`: Homebrew
// enters a single top-level directory before running install (see
// AbstractDownloadStrategy#chdir, which ignores dotfiles via Dir["*"]).
type archiveTree struct {
	strip string // "" or "dir/"
	files map[string]manifest.File
	dirs  map[string]bool
}

func newArchiveTree(a manifest.Artifact) archiveTree {
	t := archiveTree{files: map[string]manifest.File{}, dirs: map[string]bool{".": true}}
	top := map[string]bool{}
	isDir := map[string]bool{}
	for _, f := range a.Files {
		first, rest, nested := strings.Cut(f.Path, "/")
		if strings.HasPrefix(first, ".") {
			continue
		}
		top[first] = true
		if nested && rest != "" || f.Type == "dir" {
			isDir[first] = true
		}
	}
	if len(top) == 1 {
		for name := range top {
			if isDir[name] {
				t.strip = name + "/"
			}
		}
	}
	for _, f := range a.Files {
		p := f.Path
		if t.strip != "" {
			if !strings.HasPrefix(p, t.strip) {
				continue
			}
			p = strings.TrimPrefix(p, t.strip)
		}
		if p == "" {
			continue
		}
		if f.Type == "dir" {
			t.dirs[p] = true
		} else {
			t.files[p] = f
		}
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			t.dirs[d] = true
		}
	}
	return t
}

// visible maps an archive entry path to the path install sees, or "" when
// the entry is outside the directory install runs in.
func (t archiveTree) visible(p string) string {
	if t.strip == "" {
		return p
	}
	if strings.HasPrefix(p, t.strip) {
		return strings.TrimPrefix(p, t.strip)
	}
	return ""
}

// Verify checks that every path referenced by a statically analyzable
// install call in f's `def install` exists in every archive the call
// applies to, after Homebrew's single-top-level-directory rule. It returns
// an error naming each archive and missing path. Calls that cannot be
// analyzed are not errors (Generate reports them as warnings).
func Verify(f Formula) error {
	body, err := installBody(f.Content)
	if err != nil {
		return err
	}
	_, err = verifyInstall(f.Name, body, f.Archives)
	return err
}

// installBody extracts the body of `def install` from generated content.
func installBody(content []byte) (string, error) {
	lines := strings.Split(string(content), "\n")
	start := -1
	for i, l := range lines {
		if l == "  def install" {
			start = i + 1
			continue
		}
		if start >= 0 && l == "  end" {
			return strings.Join(lines[start:i], "\n"), nil
		}
	}
	return "", errors.New("formula has no def install")
}

func verifyInstall(name, body string, archives []manifest.Artifact) ([]string, error) {
	var warnings []string
	trees := make([]archiveTree, len(archives))
	for i, a := range archives {
		trees[i] = newArchiveTree(a)
		if len(a.Files) == 0 {
			warnings = append(warnings, fmt.Sprintf("formula %s: archive %s has no file list in the manifest; install paths cannot be verified against it", name, a.Name))
		}
	}
	missing := map[string][]string{} // archive name -> problems
	calls := analyzeInstall(body)
	for _, a := range archives {
		missing[a.Name] = append(missing[a.Name], duplicateInstalls(calls, a)...)
	}
	for _, c := range calls {
		where := fmt.Sprintf("install line %d", c.line)
		if c.reason != "" {
			warnings = append(warnings, fmt.Sprintf("formula %s: cannot verify %s.install on %s: %s", name, receiverName(c.receiver), where, c.reason))
			continue
		}
		for i, a := range archives {
			if len(a.Files) == 0 || !appliesTo(c.platforms, a) {
				continue
			}
			for _, src := range c.srcs {
				p := path.Clean(src)
				if strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
					missing[a.Name] = append(missing[a.Name], fmt.Sprintf("%q (%s.install, %s) is outside the archive", src, c.receiver, where))
					continue
				}
				file, isFile := trees[i].files[p]
				if !isFile && !trees[i].dirs[p] {
					hint := ""
					if trees[i].strip != "" {
						hint = fmt.Sprintf("; paths are relative to the top-level directory %q", strings.TrimSuffix(trees[i].strip, "/"))
					}
					missing[a.Name] = append(missing[a.Name], fmt.Sprintf("%q (%s.install, %s) not found%s", src, c.receiver, where, hint))
					continue
				}
				if c.receiver == "bin" || c.receiver == "sbin" {
					if !isFile {
						warnings = append(warnings, fmt.Sprintf("formula %s: %s.install %q installs a directory (archive %s)", name, c.receiver, src, a.Name))
					} else if mode, err := strconv.ParseUint(file.Mode, 8, 32); err == nil && mode&0o111 == 0 {
						warnings = append(warnings, fmt.Sprintf("formula %s: %s.install %q is not executable in archive %s (mode %s)", name, c.receiver, src, a.Name, file.Mode))
					}
				}
			}
		}
	}
	for n, ps := range missing {
		if len(ps) == 0 {
			delete(missing, n)
		}
	}
	if len(missing) == 0 {
		return warnings, nil
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "formula %s: install cannot succeed for every archive:", name)
	for _, n := range names {
		for _, p := range missing[n] {
			fmt.Fprintf(&b, "\n  %s: %s", n, p)
		}
	}
	return warnings, errors.New(b.String())
}

func receiverName(r string) string {
	if r == "" {
		return "(expression)"
	}
	return r
}

func appliesTo(preds []platformPred, a manifest.Artifact) bool {
	for _, p := range preds {
		if !p.match(a) {
			return false
		}
	}
	return true
}

// duplicateInstalls finds install calls that cannot all succeed for archive
// a, or that overwrite each other: Pathname#install moves its source, so a
// source (or a directory containing it) installed twice is gone the second
// time, and two installs to one destination clobber each other.
func duplicateInstalls(calls []installCall, a manifest.Artifact) []string {
	type use struct {
		where, receiver string
	}
	var problems []string
	srcs := map[string]use{}  // cleaned source -> first install
	dests := map[string]use{} // receiver/name -> first install
	var order []string
	for _, c := range calls {
		if c.reason != "" || c.receiver == "" || !appliesTo(c.platforms, a) {
			continue
		}
		where := fmt.Sprintf("install line %d", c.line)
		for _, src := range c.srcs {
			p := path.Clean(src)
			if prev, ok := srcs[p]; ok {
				problems = append(problems, fmt.Sprintf("%q is installed twice (%s.install, %s and %s.install, %s); the first install moves it, so the second fails: install it once and use install_symlink for another location", src, prev.receiver, prev.where, c.receiver, where))
				continue
			}
			for _, q := range order {
				if strings.HasPrefix(p, q+"/") || strings.HasPrefix(q, p+"/") {
					prev := srcs[q]
					problems = append(problems, fmt.Sprintf("%q (%s.install, %s) overlaps %q (%s.install, %s): installing a directory moves everything in it", src, c.receiver, where, q, prev.receiver, prev.where))
				}
			}
			srcs[p] = use{where, c.receiver}
			order = append(order, p)
			name := path.Base(p)
			if d, ok := c.dests[src]; ok {
				name = d
			}
			dest := c.receiver + "/" + name
			if prev, ok := dests[dest]; ok {
				problems = append(problems, fmt.Sprintf("two installs write %s (%s and %s): the second overwrites the first", dest, prev.where, where))
				continue
			}
			dests[dest] = use{where, c.receiver}
		}
	}
	return problems
}
