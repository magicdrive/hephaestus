// Command tap-version-guard fails when a change to a Homebrew tap lowers
// the version of a formula. It is meant for the tap's own CI, as a required
// check on pull requests (and the merge queue): when the pull requests of
// two releases are open at once, merging the older one last would otherwise
// downgrade the formula.
//
// It compares every changed *.rb file that exists on both sides, by the
// version the formula declares (its class-level version statement, else
// the semantic version in its url), with semantic version precedence:
// 1.10.0 > 1.9.0, 1.3.0 > 1.3.0-rc.1, build metadata ignored. A formula
// whose version cannot be compared is reported as a warning, not a failure.
//
// The default range, HEAD^1..HEAD, is what a pull_request run checks out:
// the merge of the pull request into the current base branch. The check
// is only as fresh as that merge: require branches to be up to date before
// merging (or use a merge queue), so that a pull request is checked again
// after another one moved the base branch.
//
// -against rev checks a pull request head that is not merged with the
// current base: the formulae the head changed since its merge base with
// rev are compared by their version at rev (the current tip of the base
// branch) and at head. It needs no merge commit, so it re-checks open pull
// requests after the base branch moved (a green pull_request check of an
// older base is stale, and re-running that run checks the same old merge
// commit again), and lets a reviewer re-check right before merging.
//
// -each checks every commit on the first-parent chain of base..head
// against its first parent, for a push to the base branch (before..after):
// it catches a downgrade inside a merge queue group or a fast-forward of
// several merges, whose net change may go up.
//
// -strict (for a required check) also fails, in every mode, a changed
// formula whose new side has no version or a version that is not a
// semantic version, versions that cannot be compared, the same version
// with different artifacts (url, sha256, mirror, ...: a re-release under
// the same version cannot be told from a swapped artifact), a url of
// another version than the formula's, a removed formula, and a formula
// that is not a regular file. It checks only the formulae Homebrew loads
// (Formula/**, else HomebrewFormula/**, else *.rb at the root of the tap);
// see strict.go.
//
// An intentional rollback passes with -allow-downgrade (reported as a
// warning); see docs/tap-version-guard.md. Under -strict it lowers the
// findings about which version and artifacts the tap serves (downgrade,
// incomparable versions, same-version artifact change, url version
// mismatch, removal), never those that mean the guard cannot read the
// formula (no version, not a regular file, an unsafe path, a formula
// directory that is not a directory).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/magicdrive/hephaestus/internal/tap"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tap-version-guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "the tap repository")
	base := fs.String("base", "HEAD^1", "the revision the change is applied to (the base branch)")
	head := fs.String("head", "HEAD", "the revision with the change")
	against := fs.String("against", "", "compare head with this tip of the base branch: only the formulae head changed since its merge base with it (an unmerged pull request head)")
	each := fs.Bool("each", false, "check every commit on the first-parent chain of base..head against its first parent (a push to the base branch)")
	allow := fs.Bool("allow-downgrade", false, "report downgrades as warnings (an intentional rollback); with -strict also the other version and artifact findings")
	strict := fs.Bool("strict", false, "also fail a formula with no or a non-semantic version, incomparable versions, the same version with other artifacts, a url of another version, a removed formula, a formula that is not a regular file (only the formulae Homebrew loads)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: tap-version-guard [-C dir] [-base rev [-each] | -against rev] [-head rev] [-strict] [-allow-downgrade]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *against != "" && (set["base"] || *each) {
		fmt.Fprintln(stderr, "tap-version-guard: -against excludes -base and -each")
		return 2
	}
	g := gitRepo(*dir)
	opt := options{allow: *allow, strict: *strict}
	var failed bool
	var err error
	switch {
	case *against != "":
		var files []string
		if files, err = g.changedSinceMergeBase(*against, *head); err == nil {
			failed, err = g.check(*against, *head, files, opt, stdout)
		}
	case *each:
		failed, err = g.checkEach(*base, *head, opt, stdout)
	default:
		var files []string
		if files, err = g.changedFormulae(*base, *head); err == nil {
			failed, err = g.check(*base, *head, files, opt, stdout)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "tap-version-guard: %v\n", err)
		return 2
	}
	if failed {
		return 1
	}
	return 0
}

// checkEach checks every commit on the first-parent chain of base..head
// against its first parent: a push that lands several merges (a merge
// queue group, a fast-forward) is a downgrade if any step is one, even
// when the net change is not.
func (g gitRepo) checkEach(base, head string, opt options, stdout io.Writer) (bool, error) {
	if _, err := g.changedFormulae(base, head); err != nil { // both are commits
		return false, err
	}
	out, err := g.git("rev-list", "--first-parent", "--reverse", base+".."+head)
	if err != nil {
		return false, err
	}
	failed := false
	for _, c := range strings.Fields(string(out)) {
		files, err := g.changedFormulae(c+"^1", c)
		if err != nil {
			return false, fmt.Errorf("commit %s: %w", c, err)
		}
		f, err := g.check(c+"^1", c, files, opt, stdout)
		if err != nil {
			return false, err
		}
		failed = failed || f
	}
	return failed, nil
}

// options are the flags every mode applies.
type options struct {
	allow  bool // -allow-downgrade
	strict bool // -strict
}

// check compares the version of each file at from and at head; it reports
// whether one goes down (or, with -strict, any finding fails).
func (g gitRepo) check(from, head string, files []string, opt options, stdout io.Writer) (bool, error) {
	if opt.strict {
		return g.checkStrict(from, head, files, opt.allow, stdout)
	}
	allow := opt.allow
	failed := false
	for _, f := range files {
		oldC, okOld, err := g.show(from, f)
		if err != nil {
			return false, err
		}
		newC, okNew, err := g.show(head, f)
		if err != nil {
			return false, err
		}
		if !okOld || !okNew {
			continue // added or removed
		}
		oldV, newV := tap.FormulaVersion(oldC), tap.FormulaVersion(newC)
		if oldV == "" || newV == "" || oldV == newV {
			fmt.Fprintf(stdout, "ok %s: %s -> %s\n", escapeData(f), escapeData(orNone(oldV)), escapeData(orNone(newV)))
			continue
		}
		c, ok := tap.CompareVersions(oldV, newV)
		switch {
		case !ok:
			annotate(stdout, "warning", f, fmt.Sprintf("%s: cannot compare version %q with %q (not semantic versions); check the order by hand", f, oldV, newV))
		case c > 0 && allow:
			annotate(stdout, "warning", f, fmt.Sprintf("%s: version goes down from %s to %s (allowed: -allow-downgrade)", f, oldV, newV))
		case c > 0:
			annotate(stdout, "error", f, fmt.Sprintf("%s: version goes down from %s to %s. %s", f, oldV, newV, downgradeAdvice))
			failed = true
		default:
			fmt.Fprintf(stdout, "ok %s: %s -> %s\n", escapeData(f), escapeData(oldV), escapeData(newV))
		}
	}
	return failed, nil
}

const downgradeAdvice = "A newer release was merged first; close this pull request. For an intentional rollback see the tap's rollback procedure."

// annotate prints a GitHub Actions workflow command (error or warning) for
// file. Paths and versions come from the change under test: they are
// escaped, so a crafted name cannot end the command and start another one
// (a newline followed by "::add-mask::" or "::stop-commands::").
func annotate(w io.Writer, level, file, msg string) {
	fmt.Fprintf(w, "::%s file=%s::%s\n", level, escapeProperty(file), escapeData(msg))
}

// escapeData escapes a workflow command's message as the Actions runner
// unescapes it.
func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// escapeProperty escapes a workflow command's property value.
func escapeProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

func orNone(v string) string {
	if v == "" {
		return "(no version)"
	}
	return v
}

type gitRepo string

func (g gitRepo) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", string(g)}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// changedFormulae lists the *.rb files that differ between base and head.
func (g gitRepo) changedFormulae(base, head string) ([]string, error) {
	for _, rev := range []string{base, head} {
		if _, err := g.git("rev-parse", "--verify", "--quiet", rev+"^{commit}"); err != nil {
			return nil, fmt.Errorf("not a commit: %q (fetch enough history: fetch-depth 2 for a pull request merge)", rev)
		}
	}
	out, err := g.git("diff", "--name-only", "--no-renames", "-z", base, head, "--", "*.rb")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// changedSinceMergeBase lists the *.rb files head changed since its merge
// base with tip.
func (g gitRepo) changedSinceMergeBase(tip, head string) ([]string, error) {
	for _, rev := range []string{tip, head} {
		if _, err := g.git("rev-parse", "--verify", "--quiet", rev+"^{commit}"); err != nil {
			return nil, fmt.Errorf("not a commit: %q (fetch the base branch and the pull request head)", rev)
		}
	}
	mb, err := g.git("merge-base", tip, head)
	if err != nil {
		return nil, fmt.Errorf("no merge base of %s and %s (fetch more history: fetch-depth 0): %w", tip, head, err)
	}
	return g.changedFormulae(strings.TrimSpace(string(mb)), head)
}

// show returns path at rev; ok is false when rev has no such file (or a
// submodule there). A file whose object is missing is an error, not an
// absent file.
func (g gitRepo) show(rev, path string) ([]byte, bool, error) {
	e, err := g.entry(rev, path)
	if err != nil || !e.exists {
		return nil, false, err
	}
	switch {
	case e.regular:
		return e.content, true, nil
	case e.mode == "160000": // a submodule: not a file of the tap
		return nil, false, nil
	}
	// A symlink reads as its target path; a directory is an error.
	b, err := g.git("cat-file", "blob", rev+":"+path)
	return b, err == nil, err
}
