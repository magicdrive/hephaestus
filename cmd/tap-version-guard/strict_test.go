package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/tap"
)

// brewFormula is a formula as Hephaestus generates it: no version
// statement, the version in the release download urls.
func brewFormula(v, sha string) string {
	return fmt.Sprintf(`# typed: false
# frozen_string_literal: true

class Tool < Formula
  desc "tool"
  homepage "https://example.com/tool"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/o/tool/releases/download/v%[1]s/tool_%[1]s_darwin_arm64.tar.gz"
      sha256 "%[2]sa"
    end
    on_intel do
      url "https://github.com/o/tool/releases/download/v%[1]s/tool_%[1]s_darwin_amd64.tar.gz"
      sha256 "%[2]sb"
    end
  end

  def install
    bin.install "tool"
  end

  test do
    system "#{bin}/tool", "--version"
  end
end
`, v, sha)
}

func brew(v string) string { return brewFormula(v, "0000") }

// entry adds a raw tree entry (a symlink, an odd name) to the index, with
// no file in the work tree, and commits.
func (r *tapRepo) entry(mode, path, content string) string {
	r.t.Helper()
	cmd := exec.Command("git", "-C", r.dir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatal(err)
	}
	// core.protectNTFS (on by default on Windows) refuses names with ':'
	// or control characters; they never reach a work tree here.
	r.git("-c", "core.protectNTFS=false", "update-index", "--add", "--cacheinfo", mode+","+strings.TrimSpace(string(out))+","+path)
	r.git("commit", "-q", "-m", "entry")
	return r.git("rev-parse", "HEAD")
}

type strictCase struct {
	name   string
	base   map[string]string // the base commit
	change map[string]string // the change on top ("" removes a file)
	args   []string          // besides -strict
	code   int
	want   []string // substrings of the output
	not    []string // must not occur
	// legacy is the exit status without -strict (-1: not checked), which
	// must stay what it was before -strict existed.
	legacy int
}

func swapPlatforms(f string) string {
	f = strings.Replace(f, "darwin_arm64", "TMP", 1)
	f = strings.Replace(f, "darwin_amd64", "darwin_arm64", 1)
	return strings.Replace(f, "TMP", "darwin_amd64", 1)
}

func strictCases() []strictCase {
	const tool = "tool.rb"
	return []strictCase{
		// GREEN
		{name: "upgrade", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{tool: brew("1.1.0")},
			code: 0, want: []string{"ok tool.rb: 1.0.0 -> 1.1.0"}, legacy: 0},
		{name: "upgrade with a version statement", base: map[string]string{tool: formula("1.0.0")}, change: map[string]string{tool: formula("2.0.0")},
			code: 0, legacy: 0},
		{name: "several formulae upgraded", base: map[string]string{tool: brew("1.0.0"), "other.rb": formula("0.1.0")},
			change: map[string]string{tool: brew("1.1.0"), "other.rb": formula("0.2.0")},
			code:   0, want: []string{"ok other.rb: 0.1.0 -> 0.2.0", "ok tool.rb: 1.0.0 -> 1.1.0"}, legacy: 0},
		{name: "new formula", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{"new.rb": brew("0.1.0")},
			code: 0, want: []string{"ok new.rb: added at 0.1.0"}, legacy: 0},
		{name: "release after its prerelease", base: map[string]string{tool: brew("1.3.0-rc.1")}, change: map[string]string{tool: brew("1.3.0")},
			code: 0, legacy: 0},
		{name: "prerelease order rc.2 -> rc.10", base: map[string]string{tool: brew("1.3.0-rc.2")}, change: map[string]string{tool: brew("1.3.0-rc.10")},
			code: 0, legacy: 0},
		{name: "same version, description changed", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: strings.Replace(brew("1.0.0"), `desc "tool"`, `desc "a tool"`, 1)},
			code:   0, want: []string{"ok tool.rb: 1.0.0 -> 1.0.0"}, legacy: 0},
		{name: "same version, test block changed", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: strings.Replace(brew("1.0.0"), `"--version"`, `"version"`, 1)},
			code:   0, legacy: 0},
		{name: "same version, caveats heredoc mentions a url", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: strings.Replace(brew("1.0.0"), "  def install", "  def caveats\n    <<~EOS\n      url \"https://elsewhere/\"\n    EOS\n  end\n\n  def install", 1)},
			code:   0, legacy: 0},
		{name: "non-formula Ruby in a root tap", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{"cmd/brew-x.rb": "puts 1\n", "Casks/x.rb": "cask \"x\" do\nend\n", ".github/x.rb": "x\n"},
			code:   0, want: []string{"skip cmd/brew-x.rb: not a formula", "skip Casks/x.rb", "skip .github/x.rb"}, legacy: 0},
		{name: "Formula/ tap, sharded", base: map[string]string{"Formula/t/tool.rb": brew("1.0.0")},
			change: map[string]string{"Formula/t/tool.rb": brew("1.1.0"), "Formula/n.rb": brew("0.1.0")},
			code:   0, want: []string{"ok Formula/n.rb: added at 0.1.0", "ok Formula/t/tool.rb: 1.0.0 -> 1.1.0"}, legacy: 0},
		{name: "Formula/ tap ignores root *.rb", base: map[string]string{"Formula/tool.rb": brew("1.0.0"), "stray.rb": brew("2.0.0")},
			change: map[string]string{"stray.rb": "garbage\n"},
			code:   0, want: []string{"skip stray.rb: not a formula (Homebrew loads Formula/**/*.rb)"}, legacy: 0},
		{name: "HomebrewFormula/ tap", base: map[string]string{"HomebrewFormula/tool.rb": brew("1.0.0")},
			change: map[string]string{"HomebrewFormula/tool.rb": brew("0.9.0")},
			code:   1, want: []string{"version goes down from 1.0.0 to 0.9.0"}, legacy: 1},

		// RED
		{name: "downgrade", base: map[string]string{tool: brew("2.0.0")}, change: map[string]string{tool: brew("1.5.0")},
			code: 1, want: []string{"::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0. A newer release"}, legacy: 1},
		{name: "prerelease downgrade", base: map[string]string{tool: brew("1.3.0")}, change: map[string]string{tool: brew("1.3.0-rc.1")},
			code: 1, want: []string{"goes down from 1.3.0 to 1.3.0-rc.1"}, legacy: 1},
		{name: "prerelease downgrade rc.10 -> rc.9", base: map[string]string{tool: brew("1.3.0-rc.10")}, change: map[string]string{tool: brew("1.3.0-rc.9")},
			code: 1, want: []string{"goes down from 1.3.0-rc.10 to 1.3.0-rc.9"}, legacy: 1},
		{name: "same version, sha256 changed", base: map[string]string{tool: brewFormula("1.0.0", "1111")}, change: map[string]string{tool: brewFormula("1.0.0", "2222")},
			code: 1, want: []string{"tool.rb: version 1.0.0 is unchanged but its artifacts changed (sha256)", "Release a new version"}, legacy: 0},
		{name: "same version, url changed", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: strings.Replace(brew("1.0.0"), "github.com/o/", "evil.example/o/", 1)},
			code:   1, want: []string{"artifacts changed (url)"}, legacy: 0},
		{name: "same version, platforms swapped", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{tool: swapPlatforms(brew("1.0.0"))},
			code: 1, want: []string{"artifacts changed (url)"}, legacy: 0},
		{name: "same version, mirror added", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: strings.Replace(brew("1.0.0"), "      sha256 \"0000a\"", "      mirror \"https://m.example/t.tar.gz\"\n      sha256 \"0000a\"", 1)},
			code:   1, want: []string{"artifacts changed (mirror)"}, legacy: 0},
		{name: "same version, resource changed", base: map[string]string{tool: formula("1.0.0")},
			change: map[string]string{tool: strings.Replace(formula("1.0.0"), "end\n", "  resource \"x\" do\n    url \"https://x.example/x.tgz\"\n  end\nend\n", 1)},
			code:   1, want: []string{"artifacts changed (resource, url)"}, legacy: 0},
		{name: "same version, patch data changed", base: map[string]string{tool: formula("1.0.0") + "__END__\n-a\n"},
			change: map[string]string{tool: formula("1.0.0") + "__END__\n-b\n"},
			code:   1, want: []string{"artifacts changed (__END__ data)"}, legacy: 0},
		{name: "same version, build metadata and sha256", base: map[string]string{tool: strings.Replace(formula("1.0.0+b1"), "end\n", "  sha256 \"1\"\nend\n", 1)},
			change: map[string]string{tool: strings.Replace(formula("1.0.0+b2"), "end\n", "  sha256 \"2\"\nend\n", 1)},
			code:   1, want: []string{"version 1.0.0+b2 is unchanged but its artifacts changed (sha256)"}, legacy: 0},
		{name: "no version", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{tool: "class Tool < Formula\n  url \"https://example.com/tool.tar.gz\"\nend\n"},
			code: 1, want: []string{"tool.rb: no version", "a malformed formula"}, legacy: 0},
		{name: "no version, rollback label does not help", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{tool: "garbage\n"},
			args: []string{"-allow-downgrade"}, code: 1, want: []string{"::error file=tool.rb::tool.rb: no version"}, legacy: 0},
		{name: "added without a version", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{"new.rb": "class New < Formula\nend\n"},
			code: 1, want: []string{"new.rb: no version"}, legacy: 0},
		{name: "added with a version that is not semantic", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{"new.rb": formula("2024.1")},
			code: 1, want: []string{`new.rb: version "2024.1" is not a semantic version`}, legacy: 0},
		{name: "versions that cannot be compared", base: map[string]string{tool: formula("2024.1")}, change: map[string]string{tool: formula("2023.9")},
			code: 1, want: []string{`version "2023.9" is not a semantic version`}, legacy: 0},
		{name: "old side without version", base: map[string]string{tool: "class Tool < Formula\nend\n"}, change: map[string]string{tool: brew("1.0.0")},
			code: 1, want: []string{"cannot compare version (no version) with 1.0.0"}, legacy: 0},
		{name: "url of another version", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: strings.Replace(brew("1.1.0"), "v1.1.0/tool_1.1.0_darwin_amd64", "v1.0.0/tool_1.0.0_darwin_amd64", 1)},
			code:   1, want: []string{"url https://github.com/o/tool/releases/download/v1.0.0/tool_1.0.0_darwin_amd64.tar.gz is of version 1.0.0, the formula's version is 1.1.0"}, legacy: 0},
		{name: "removed", base: map[string]string{tool: brew("1.0.0"), "other.rb": brew("1.0.0")}, change: map[string]string{tool: ""},
			code: 1, want: []string{"tool.rb: formula removed (was 1.0.0)"}, legacy: 0},
		{name: "moved to Formula/ (layout change)", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{tool: "", "Formula/tool.rb": brew("1.0.0")},
			code:   1, want: []string{"tool.rb: formula removed (was 1.0.0)", "ok Formula/tool.rb: added at 1.0.0"}, legacy: 0},
		{name: "Formula/ created, root formula left behind", base: map[string]string{tool: brew("1.0.0")},
			change: map[string]string{"Formula/new.rb": brew("0.1.0")},
			code:   1, want: []string{"tool.rb: formula removed (was 1.0.0)"}, legacy: 0},

		// -allow-downgrade (the formula-rollback label)
		{name: "rollback allowed", base: map[string]string{tool: brew("2.0.0")}, change: map[string]string{tool: brew("1.5.0")},
			args: []string{"-allow-downgrade"}, code: 0, want: []string{"::warning file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0 (allowed: -allow-downgrade)"}, not: []string{"::error"}, legacy: -1},
		{name: "re-release allowed", base: map[string]string{tool: brewFormula("1.0.0", "1111")}, change: map[string]string{tool: brewFormula("1.0.0", "2222")},
			args: []string{"-allow-downgrade"}, code: 0, want: []string{"::warning file=tool.rb::tool.rb: version 1.0.0 is unchanged but its artifacts changed (sha256)", "(allowed"}, legacy: -1},
		{name: "removal allowed", base: map[string]string{tool: brew("1.0.0")}, change: map[string]string{tool: ""},
			args: []string{"-allow-downgrade"}, code: 0, want: []string{"::warning file=tool.rb::tool.rb: formula removed"}, legacy: -1},
		{name: "not semantic allowed", base: map[string]string{tool: formula("2024.1")}, change: map[string]string{tool: formula("2023.9")},
			args: []string{"-allow-downgrade"}, code: 0, want: []string{"::warning"}, legacy: -1},
	}
}

func TestStrict(t *testing.T) {
	for _, tc := range strictCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTap(t)
			r.commit(map[string]string{"README.md": "tap"})
			if len(tc.base) > 0 {
				r.commit(tc.base)
			}
			r.commit(tc.change)
			code, out := r.guard(append([]string{"-strict"}, tc.args...)...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(out, w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
			if tc.code != 0 && !strings.Contains(out, "::error") {
				t.Errorf("a failure without an ::error annotation:\n%s", out)
			}
			if tc.legacy >= 0 {
				if code, out := r.guard(tc.args...); code != tc.legacy {
					t.Errorf("without -strict: exit %d, want %d (unchanged):\n%s", code, tc.legacy, out)
				}
			}
		})
	}
}

// TestStrictEntries: formulae that are not regular files, unsafe paths
// and a formula directory that is not a directory fail, with or without
// the rollback label: the guard cannot read what Homebrew would load.
func TestStrictEntries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		mode, path, data string
		want             string
	}{
		{"symlink formula", "120000", "tool.rb", "old/tool.rb", "tool.rb: not a regular file (mode 120000)"},
		{"symlink formula in Formula/", "120000", "Formula/x.rb", "../x.rb", "Formula/x.rb: not a regular file"},
		{"submodule named like a formula", "160000", "sub.rb", "", "sub.rb: not a regular file (mode 160000)"},
		{"newline in the name", "100644", "x\n::warning::pwned.rb", formula("1.0.0"), "control character in the path"},
		{"Formula is a symlink", "120000", "Formula", "elsewhere", "::error file=Formula::Formula is not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, allow := range []bool{false, true} {
				r := newTap(t)
				r.commit(map[string]string{"README.md": "tap", "tool.rb": brew("1.0.0")})
				if tc.mode == "160000" {
					tc.data = r.git("rev-parse", "HEAD")
					r.git("update-index", "--add", "--cacheinfo", "160000,"+tc.data+","+tc.path)
					r.git("commit", "-q", "-m", "submodule")
				} else {
					r.entry(tc.mode, tc.path, tc.data)
				}
				args := []string{"-strict"}
				if allow {
					args = append(args, "-allow-downgrade")
				}
				code, out := r.guard(args...)
				if code != 1 || !strings.Contains(out, tc.want) || !strings.Contains(out, "::error") {
					t.Fatalf("allow=%v: exit %d, want 1 with %q:\n%s", allow, code, tc.want, out)
				}
				for _, line := range strings.Split(out, "\n") {
					if strings.HasPrefix(line, "::warning::pwned") {
						t.Fatalf("a path started a workflow command:\n%s", out)
					}
				}
			}
		})
	}
}

// TestDefaultModeEscapesWorkflowCommands: without -strict too, a crafted
// file name cannot start a workflow command of its own.
func TestDefaultModeEscapesWorkflowCommands(t *testing.T) {
	r := newTap(t)
	name := "x\n::add-mask::secret,%0A.rb"
	r.entry("100644", name, formula("2.0.0"))
	r.entry("100644", name, formula("1.0.0"))
	code, out := r.guard()
	want := "::error file=x%0A%3A%3Aadd-mask%3A%3Asecret%2C%250A.rb::x%0A::add-mask::secret,%250A.rb: version goes down from 2.0.0 to 1.0.0. "
	if code != 1 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, want) {
		t.Fatalf("exit %d, want 1 and one line starting %q:\n%s", code, want, out)
	}
}

// TestTOCTOU reproduces the race of two release pull requests on a local
// tap (base 1.0.0, A: 2.0.0, B: 1.5.0, both opened on the same base):
// both are green against the old base; once A is merged, every way B can
// still land is red with -strict.
func TestTOCTOU(t *testing.T) {
	const tool = "tool.rb" // a tap with formulae at its root, like magicdrive/homebrew-tap
	r := newTap(t)
	r.commit(map[string]string{tool: brew("1.0.0"), "README.md": "tap"})
	oldBase := r.git("rev-parse", "main")
	for _, pr := range []struct{ branch, v string }{{"pr-a", "2.0.0"}, {"pr-b", "1.5.0"}} {
		r.git("checkout", "-q", "-b", pr.branch, "main")
		r.commit(map[string]string{tool: brew(pr.v)})
	}
	r.git("checkout", "-q", "main")

	// Both green against the old base: the head, the head against the tip,
	// and the pull_request merge commit (HEAD^1..HEAD).
	for _, b := range []string{"pr-a", "pr-b"} {
		if code, out := r.guard("-strict", "-base", "main", "-head", b); code != 0 {
			t.Fatalf("%s against the old base: exit %d\n%s", b, code, out)
		}
		if code, out := r.guard("-strict", "-against", "main", "-head", b); code != 0 {
			t.Fatalf("%s -against the old base: exit %d\n%s", b, code, out)
		}
		r.git("checkout", "-q", "-b", "merge-"+b, oldBase)
		r.git("merge", "-q", "--no-ff", "--no-edit", b)
		if code, out := r.guard("-strict"); code != 0 {
			t.Fatalf("merge of %s into the old base: exit %d\n%s", b, code, out)
		}
	}

	// A is merged (the merge button: a merge commit).
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "--no-edit", "pr-a")

	t.Run("B -against the new tip", func(t *testing.T) {
		code, out := r.guard("-strict", "-against", "main", "-head", "pr-b")
		if code != 1 || !strings.Contains(out, "::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("B merged into the new base, conflict resolved for B", func(t *testing.T) {
		r.git("checkout", "-q", "-b", "fresh-merge-b", "main")
		if err := exec.Command("git", "-C", r.dir, "merge", "-q", "--no-edit", "pr-b").Run(); err == nil {
			t.Fatal("B merged into the new base without a conflict")
		}
		r.git("checkout", "--theirs", "--", tool)
		r.git("add", tool)
		r.git("commit", "-q", "--no-edit")
		code, out := r.guard("-strict")
		if code != 1 || !strings.Contains(out, "goes down from 2.0.0 to 1.5.0") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		r.git("checkout", "-q", "main")
	})
	t.Run("A then B pushed together, -each", func(t *testing.T) {
		r.git("checkout", "-q", "-b", "pushed", "main")
		if err := exec.Command("git", "-C", r.dir, "merge", "-q", "-X", "theirs", "--no-ff", "--no-edit", "pr-b").Run(); err != nil {
			t.Fatal(err)
		}
		code, out := r.guard("-strict", "-base", oldBase, "-head", "pushed", "-each")
		if code != 1 || !strings.Contains(out, "goes down from 2.0.0 to 1.5.0") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		// The net change of the push goes up (1.0.0 -> 1.5.0): only -each
		// sees the step down.
		if code, out := r.guard("-strict", "-base", oldBase, "-head", "pushed"); code != 0 {
			t.Fatalf("net change: exit %d\n%s", code, out)
		}
		r.git("checkout", "-q", "main")
	})
	t.Run("the stale check stays green", func(t *testing.T) {
		// Re-running B's old pull_request run checks the merge commit it
		// checked before: green. Only a fresh merge (strict up-to-date
		// rule, pr-gate's base.up_to_date) or -against sees the new base.
		r.git("checkout", "-q", "merge-pr-b")
		if code, out := r.guard("-strict"); code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		r.git("checkout", "-q", "main")
	})
	t.Run("rollback label", func(t *testing.T) {
		code, out := r.guard("-strict", "-against", "main", "-head", "pr-b", "-allow-downgrade")
		if code != 0 || !strings.Contains(out, "::warning file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0 (allowed") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}

// TestConcurrentPullRequests merges release pull requests opened on one
// base in different orders. Before each merge the next pull request is
// checked -against the tip and, merged into it, HEAD^1..HEAD; both must be
// red exactly when it is older than what is on main, and a red one is not
// merged (the required check blocks it).
func TestConcurrentPullRequests(t *testing.T) {
	for _, order := range [][]string{
		{"1.1.0", "1.2.0", "2.0.0"},
		{"2.0.0", "1.2.0", "1.1.0"},
		{"1.2.0", "2.0.0", "1.1.0"},
		{"2.0.0-rc.1", "2.0.0"},
		{"2.0.0", "2.0.0-rc.1"},
		{"2.0.0-rc.2", "2.0.0-rc.10", "2.0.0-rc.9"},
	} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			t.Parallel()
			r := releasePRsOf(t, brew, "1.0.0", order...)
			top := "1.0.0"
			for _, v := range order {
				red := less(t, v, top)
				want := 0
				if red {
					want = 1
				}
				if code, out := r.guard("-strict", "-against", "main", "-head", "tool-"+v); code != want {
					t.Fatalf("%s -against main at %s: exit %d, want %d\n%s", v, top, code, want, out)
				}
				r.git("checkout", "-q", "-b", "check-"+v, "main")
				if err := exec.Command("git", "-C", r.dir, "merge", "-q", "-X", "theirs", "--no-ff", "--no-edit", "tool-"+v).Run(); err != nil {
					t.Fatal(err)
				}
				if code, out := r.guard("-strict"); code != want {
					t.Fatalf("%s merged into main at %s: exit %d, want %d\n%s", v, top, code, want, out)
				}
				r.git("checkout", "-q", "main")
				if !red {
					r.git("merge", "-q", "--ff-only", "check-"+v)
					top = v
				}
			}
		})
	}
	t.Run("different formulae", func(t *testing.T) {
		r := newTap(t)
		r.commit(map[string]string{"a.rb": brew("1.0.0"), "b.rb": brew("1.0.0")})
		for _, f := range []string{"a", "b"} {
			r.git("checkout", "-q", "-b", "pr-"+f, "main")
			r.commit(map[string]string{f + ".rb": brew("2.0.0")})
		}
		r.git("checkout", "-q", "main")
		r.git("merge", "-q", "--no-ff", "--no-edit", "pr-a")
		if code, out := r.guard("-strict", "-against", "main", "-head", "pr-b"); code != 0 || strings.Contains(out, "a.rb") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}

func less(t *testing.T, a, b string) bool {
	t.Helper()
	c, ok := tap.CompareVersions(a, b)
	if !ok {
		t.Fatalf("cannot compare %s and %s", a, b)
	}
	return c < 0
}

// releasePRsOf is releasePRs with tool.rb at the root, made by mk.
func releasePRsOf(t *testing.T, mk func(string) string, base string, versions ...string) *tapRepo {
	t.Helper()
	r := newTap(t)
	r.commit(map[string]string{"tool.rb": mk(base), "README.md": "tap"})
	for _, v := range versions {
		r.git("checkout", "-q", "-b", "tool-"+v, "main")
		r.commit(map[string]string{"tool.rb": mk(v)})
	}
	r.git("checkout", "-q", "main")
	return r
}

// TestGuardFailures: when the guard cannot read what it checks it exits
// 2, never 0, in every mode.
func TestGuardFailures(t *testing.T) {
	setup := func(t *testing.T) *tapRepo {
		r := newTap(t)
		r.commit(map[string]string{"README.md": "tap", "tool.rb": brew("1.0.0")})
		r.commit(map[string]string{"tool.rb": brew("1.1.0")})
		return r
	}
	t.Run("unknown revision", func(t *testing.T) {
		r := setup(t)
		for _, args := range [][]string{
			{"-base", "nope"}, {"-head", "nope"}, {"-against", "nope"}, {"-base", "nope", "-each"},
		} {
			for _, strict := range []bool{false, true} {
				a := args
				if strict {
					a = append([]string{"-strict"}, args...)
				}
				if code, out := r.guard(a...); code != 2 {
					t.Errorf("%v: exit %d\n%s", a, code, out)
				}
			}
		}
	})
	t.Run("shallow history", func(t *testing.T) {
		r := newTap(t)
		r.commit(map[string]string{"tool.rb": brew("1.0.0")})
		if code, out := r.guard("-strict"); code != 2 || !strings.Contains(out, "fetch-depth 2") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("missing object", func(t *testing.T) {
		for _, strict := range []bool{false, true} {
			r := setup(t)
			blob := r.git("rev-parse", "HEAD~1:tool.rb")
			obj := filepath.Join(r.dir, ".git", "objects", blob[:2], blob[2:])
			if err := os.Chmod(obj, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(obj); err != nil {
				t.Fatal(err)
			}
			var args []string
			if strict {
				args = []string{"-strict"}
			}
			if code, out := r.guard(args...); code != 2 {
				t.Fatalf("strict=%v: exit %d, want 2\n%s", strict, code, out)
			}
		}
	})
	t.Run("git not found", func(t *testing.T) {
		r := setup(t)
		t.Setenv("PATH", t.TempDir())
		for _, args := range [][]string{nil, {"-strict"}, {"-strict", "-against", "main"}} {
			if code, out := r.guard(args...); code != 2 {
				t.Errorf("%v: exit %d\n%s", args, code, out)
			}
		}
	})
	t.Run("root commit in -each", func(t *testing.T) {
		r := setup(t)
		r.git("checkout", "-q", "--orphan", "other")
		r.commit(map[string]string{"tool.rb": brew("0.1.0")})
		if code, out := r.guard("-strict", "-base", "main", "-head", "other", "-each"); code != 2 {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	if runtime.GOOS == "windows" {
		return
	}
	t.Run("unreadable repository", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads anything")
		}
		r := setup(t)
		objects := filepath.Join(r.dir, ".git", "objects")
		if err := os.Chmod(objects, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(objects, 0o755) })
		if code, out := r.guard("-strict"); code != 2 {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}
