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
// An intentional rollback passes with -allow-downgrade (reported as a
// warning); see docs/tap-version-guard.md.
package main

import (
	"bytes"
	"errors"
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
	allow := fs.Bool("allow-downgrade", false, "report downgrades as warnings (an intentional rollback)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: tap-version-guard [-C dir] [-base rev [-each] | -against rev] [-head rev] [-allow-downgrade]")
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
	var failed bool
	var err error
	switch {
	case *against != "":
		var files []string
		if files, err = g.changedSinceMergeBase(*against, *head); err == nil {
			failed, err = g.check(*against, *head, files, *allow, stdout)
		}
	case *each:
		failed, err = g.checkEach(*base, *head, *allow, stdout)
	default:
		var files []string
		if files, err = g.changedFormulae(*base, *head); err == nil {
			failed, err = g.check(*base, *head, files, *allow, stdout)
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
func (g gitRepo) checkEach(base, head string, allow bool, stdout io.Writer) (bool, error) {
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
		f, err := g.check(c+"^1", c, files, allow, stdout)
		if err != nil {
			return false, err
		}
		failed = failed || f
	}
	return failed, nil
}

// check compares the version of each file at from and at head; it reports
// whether one goes down.
func (g gitRepo) check(from, head string, files []string, allow bool, stdout io.Writer) (bool, error) {
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
			fmt.Fprintf(stdout, "ok %s: %s -> %s\n", f, orNone(oldV), orNone(newV))
			continue
		}
		c, ok := tap.CompareVersions(oldV, newV)
		switch {
		case !ok:
			fmt.Fprintf(stdout, "::warning file=%s::%s: cannot compare version %q with %q (not semantic versions); check the order by hand\n", f, f, oldV, newV)
		case c > 0 && allow:
			fmt.Fprintf(stdout, "::warning file=%s::%s: version goes down from %s to %s (allowed: -allow-downgrade)\n", f, f, oldV, newV)
		case c > 0:
			fmt.Fprintf(stdout, "::error file=%s::%s: version goes down from %s to %s. A newer release was merged first; close this pull request. For an intentional rollback see the tap's rollback procedure.\n", f, f, oldV, newV)
			failed = true
		default:
			fmt.Fprintf(stdout, "ok %s: %s -> %s\n", f, oldV, newV)
		}
	}
	return failed, nil
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

// show returns path at rev; ok is false when rev has no such file.
func (g gitRepo) show(rev, path string) ([]byte, bool, error) {
	if _, err := g.git("cat-file", "-e", rev+":"+path); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, false, nil
		}
		return nil, false, err
	}
	b, err := g.git("cat-file", "blob", rev+":"+path)
	return b, err == nil, err
}
