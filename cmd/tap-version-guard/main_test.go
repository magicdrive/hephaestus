package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func formula(version string) string {
	return "class Tool < Formula\n  desc \"tool\"\n  version \"" + version + "\"\nend\n"
}

type tapRepo struct {
	t   *testing.T
	dir string
}

func newTap(t *testing.T) *tapRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &tapRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "t@example.com")
	r.git("config", "user.name", "t")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *tapRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *tapRepo) commit(files map[string]string) string {
	r.t.Helper()
	for p, c := range files {
		full := filepath.Join(r.dir, filepath.FromSlash(p))
		if c == "" {
			r.git("rm", "-q", p)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			r.t.Fatal(err)
		}
		r.git("add", p)
	}
	r.git("commit", "-q", "--allow-empty", "-m", "c")
	return r.git("rev-parse", "HEAD")
}

func (r *tapRepo) guard(args ...string) (int, string) {
	var out, errb strings.Builder
	code := run(append([]string{"-C", r.dir}, args...), &out, &errb)
	return code, out.String() + errb.String()
}

func TestGuardTransitions(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		code     int
		want     string
	}{
		{"1.2.0", "1.3.0", 0, "ok Formula/tool.rb: 1.2.0 -> 1.3.0"},
		{"1.3.0", "1.2.0", 1, "::error file=Formula/tool.rb::Formula/tool.rb: version goes down from 1.3.0 to 1.2.0"},
		{"1.3.0-rc.1", "1.3.0", 0, "ok"},
		{"1.3.0", "1.3.0-rc.1", 1, "goes down from 1.3.0 to 1.3.0-rc.1"},
		{"1.3.0-rc.2", "1.3.0-rc.10", 0, "ok"},
		{"1.9.0", "1.10.0", 0, "ok"},
		{"1.10.0", "1.9.0", 1, "goes down from 1.10.0 to 1.9.0"},
		{"1.3.0+build.2", "1.3.0+build.1", 0, "ok"},
		{"2024.1", "2023.9", 0, "::warning file=Formula/tool.rb::Formula/tool.rb: cannot compare"},
	} {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			r := newTap(t)
			r.commit(map[string]string{"Formula/tool.rb": formula(tc.from), "README.md": "tap"})
			r.commit(map[string]string{"Formula/tool.rb": formula(tc.to)})
			code, out := r.guard()
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
		})
	}
}

func TestGuardAllowDowngrade(t *testing.T) {
	r := newTap(t)
	r.commit(map[string]string{"Formula/tool.rb": formula("1.3.0")})
	r.commit(map[string]string{"Formula/tool.rb": formula("1.2.0")})
	if code, out := r.guard("-allow-downgrade"); code != 0 || !strings.Contains(out, "::warning file=Formula/tool.rb::Formula/tool.rb: version goes down from 1.3.0 to 1.2.0 (allowed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestGuardAddedRemovedAndOtherFiles(t *testing.T) {
	r := newTap(t)
	r.commit(map[string]string{"Formula/old.rb": formula("3.0.0"), "README.md": "a"})
	r.commit(map[string]string{"Formula/old.rb": "", "Formula/a/new.rb": formula("0.1.0"), "README.md": "b"})
	if code, out := r.guard(); code != 0 || strings.Contains(out, "::error") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// TestGuardMergeOrder: the pull requests of v1.2.0 and v1.3.0 were opened
// against the same base. v1.3.0 is merged first; v1.2.0, brought up to
// date with the base (or merged by the pull_request merge commit), fails.
// The opposite order passes.
func TestGuardMergeOrder(t *testing.T) {
	r := newTap(t)
	r.commit(map[string]string{"Formula/tool.rb": formula("1.1.0")})
	r.git("branch", "tool-1.2.0")
	r.git("branch", "tool-1.3.0")
	r.git("checkout", "-q", "tool-1.2.0")
	r.commit(map[string]string{"Formula/tool.rb": formula("1.2.0")})
	r.git("checkout", "-q", "tool-1.3.0")
	r.commit(map[string]string{"Formula/tool.rb": formula("1.3.0")})

	// Against the old base, both pass (a stale check).
	for _, b := range []string{"tool-1.2.0", "tool-1.3.0"} {
		if code, out := r.guard("-base", "main", "-head", b); code != 0 {
			t.Fatalf("%s against the old base: exit %d\n%s", b, code, out)
		}
	}
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--ff-only", "tool-1.3.0")
	// The merge of tool-1.2.0 into the moved base, as a pull_request run
	// checks it out (conflicting lines resolved in the older branch's
	// favour, as a careless resolution would).
	r.git("checkout", "-q", "-b", "pr-merge", "main")
	if err := exec.Command("git", "-C", r.dir, "merge", "-q", "-X", "theirs", "--no-edit", "tool-1.2.0").Run(); err != nil {
		t.Fatal(err)
	}
	if code, out := r.guard(); code != 1 || !strings.Contains(out, "goes down from 1.3.0 to 1.2.0") {
		t.Fatalf("older merged after newer: exit %d\n%s", code, out)
	}
	// Explicit range, as a merge queue or a push check would use.
	if code, _ := r.guard("-base", "main", "-head", "pr-merge"); code != 1 {
		t.Fatalf("explicit range: exit %d", code)
	}
}

func TestGuardBadInput(t *testing.T) {
	r := newTap(t)
	r.commit(map[string]string{"README.md": "a"})
	if code, out := r.guard(); code != 2 || !strings.Contains(out, "fetch-depth 2") {
		t.Fatalf("no parent: exit %d\n%s", code, out)
	}
	if code, _ := r.guard("extra"); code != 2 {
		t.Fatalf("extra argument: exit %d", code)
	}
}

// releasePRs builds a tap whose main has tool at base and one branch per
// version, each opened against that main.
func releasePRs(t *testing.T, base string, versions ...string) *tapRepo {
	t.Helper()
	r := newTap(t)
	r.commit(map[string]string{"Formula/tool.rb": formula(base), "README.md": "tap"})
	for _, v := range versions {
		r.git("checkout", "-q", "-b", "tool-"+v, "main")
		r.commit(map[string]string{"Formula/tool.rb": formula(v)})
	}
	r.git("checkout", "-q", "main")
	return r
}

// TestGuardAgainst: -against checks an unmerged pull request head against
// the current tip of its base branch, for every order of events a stale
// pull_request check misses.
func TestGuardAgainst(t *testing.T) {
	t.Run("concurrent pull requests, nothing merged yet", func(t *testing.T) {
		r := releasePRs(t, "1.1.0", "1.2.0", "1.3.0")
		for _, b := range []string{"tool-1.2.0", "tool-1.3.0"} {
			if code, out := r.guard("-against", "main", "-head", b); code != 0 {
				t.Fatalf("%s: exit %d\n%s", b, code, out)
			}
		}
	})
	t.Run("newer already merged", func(t *testing.T) {
		r := releasePRs(t, "1.1.0", "1.2.0", "1.3.0")
		oldMain := r.git("rev-parse", "main")
		r.git("merge", "-q", "--ff-only", "tool-1.3.0")
		code, out := r.guard("-against", "main", "-head", "tool-1.2.0")
		if code != 1 || !strings.Contains(out, "goes down from 1.3.0 to 1.2.0") {
			t.Fatalf("older after newer: exit %d\n%s", code, out)
		}
		if code, out := r.guard("-against", "main", "-head", "tool-1.3.0"); code != 0 || out != "" {
			t.Fatalf("the merged head changes nothing since its merge base: exit %d\n%s", code, out)
		}
		// A re-run of the stale pull_request run checks the merge commit
		// it checked before (the old base): it passes. Hence -against.
		r.git("checkout", "-q", "-b", "stale-merge", oldMain)
		r.git("merge", "-q", "--no-ff", "--no-edit", "tool-1.2.0")
		if code, out := r.guard(); code != 0 {
			t.Fatalf("stale merge commit: exit %d\n%s", code, out)
		}
		// The fresh merge into the moved base conflicts: GitHub cannot
		// merge it until someone resolves it, which pushes a new commit
		// (synchronize) and runs the check again.
		r.git("checkout", "-q", "-b", "fresh-merge", "main")
		if err := exec.Command("git", "-C", r.dir, "merge", "-q", "--no-edit", "tool-1.2.0").Run(); err == nil {
			t.Fatal("the older release merged into the newer without a conflict")
		}
		r.git("merge", "--abort")
	})
	t.Run("pull request that does not touch the formula", func(t *testing.T) {
		r := releasePRs(t, "1.1.0", "1.3.0")
		r.git("checkout", "-q", "-b", "docs", "main")
		r.commit(map[string]string{"README.md": "more"})
		r.git("checkout", "-q", "main")
		r.git("merge", "-q", "--ff-only", "tool-1.3.0")
		// Its head still has 1.1.0, but it does not change the formula:
		// merging it keeps 1.3.0.
		if code, out := r.guard("-against", "main", "-head", "docs"); code != 0 || strings.Contains(out, "tool.rb") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("base branch moved (retargeted)", func(t *testing.T) {
		r := releasePRs(t, "1.1.0", "1.2.0")
		r.git("checkout", "-q", "-b", "next", "main")
		r.commit(map[string]string{"Formula/tool.rb": formula("2.0.0")})
		r.git("checkout", "-q", "main")
		if code, _ := r.guard("-against", "main", "-head", "tool-1.2.0"); code != 0 {
			t.Fatalf("against main: exit %d", code)
		}
		if code, out := r.guard("-against", "next", "-head", "tool-1.2.0"); code != 1 || !strings.Contains(out, "from 2.0.0 to 1.2.0") {
			t.Fatalf("against next: exit %d\n%s", code, out)
		}
	})
	t.Run("prerelease order", func(t *testing.T) {
		r := releasePRs(t, "1.2.0", "1.3.0-rc.2", "1.3.0")
		r.git("merge", "-q", "--ff-only", "tool-1.3.0")
		if code, out := r.guard("-against", "main", "-head", "tool-1.3.0-rc.2"); code != 1 || !strings.Contains(out, "from 1.3.0 to 1.3.0-rc.2") {
			t.Fatalf("rc after the release: exit %d\n%s", code, out)
		}
		r = releasePRs(t, "1.2.0", "1.3.0-rc.10", "1.3.0-rc.9")
		r.git("merge", "-q", "--ff-only", "tool-1.3.0-rc.10")
		if code, out := r.guard("-against", "main", "-head", "tool-1.3.0-rc.9"); code != 1 || !strings.Contains(out, "from 1.3.0-rc.10 to 1.3.0-rc.9") {
			t.Fatalf("rc.9 after rc.10: exit %d\n%s", code, out)
		}
		r = releasePRs(t, "1.2.0", "1.3.0-rc.1", "1.3.0")
		r.git("merge", "-q", "--ff-only", "tool-1.3.0-rc.1")
		if code, out := r.guard("-against", "main", "-head", "tool-1.3.0"); code != 0 {
			t.Fatalf("the release after its rc: exit %d\n%s", code, out)
		}
	})
	t.Run("semver, not string order", func(t *testing.T) {
		r := releasePRs(t, "1.8.0", "1.9.0", "1.10.0")
		r.git("merge", "-q", "--ff-only", "tool-1.10.0")
		if code, out := r.guard("-against", "main", "-head", "tool-1.9.0"); code != 1 || !strings.Contains(out, "from 1.10.0 to 1.9.0") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("rollback allowed", func(t *testing.T) {
		r := releasePRs(t, "1.1.0", "1.2.0", "1.3.0")
		r.git("merge", "-q", "--ff-only", "tool-1.3.0")
		if code, out := r.guard("-against", "main", "-head", "tool-1.2.0", "-allow-downgrade"); code != 0 || !strings.Contains(out, "allowed") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("bad input", func(t *testing.T) {
		r := releasePRs(t, "1.1.0", "1.2.0")
		if code, out := r.guard("-against", "main", "-base", "main", "-head", "tool-1.2.0"); code != 2 || !strings.Contains(out, "excludes") {
			t.Fatalf("both: exit %d\n%s", code, out)
		}
		if code, _ := r.guard("-against", "nope", "-head", "tool-1.2.0"); code != 2 {
			t.Fatalf("unknown tip: exit %d", code)
		}
		r.git("checkout", "-q", "--orphan", "unrelated")
		r.commit(map[string]string{"Formula/tool.rb": formula("0.1.0")})
		if code, out := r.guard("-against", "main", "-head", "unrelated"); code != 2 || !strings.Contains(out, "no merge base") {
			t.Fatalf("unrelated histories: exit %d\n%s", code, out)
		}
	})
}

// TestGuardMergeQueue: a merge queue entry stacks each pull request on the
// entries ahead of it. HEAD^1..HEAD (the entry's own change) catches an
// older release queued behind a newer one; base_sha..HEAD (the group's net
// change against the base branch) does not, so the example workflow runs
// both.
func TestGuardMergeQueue(t *testing.T) {
	r := releasePRs(t, "1.1.0", "1.2.0", "1.3.0")
	base := r.git("rev-parse", "main")
	r.git("checkout", "-q", "-b", "queue", "main")
	r.git("merge", "-q", "--no-ff", "--no-edit", "tool-1.3.0") // entry 1
	if code, _ := r.guard(); code != 0 {
		t.Fatalf("entry 1: exit %d", code)
	}
	// entry 2, its conflict resolved for the older release
	if err := exec.Command("git", "-C", r.dir, "merge", "-q", "-X", "theirs", "--no-ff", "--no-edit", "tool-1.2.0").Run(); err != nil {
		t.Fatal(err)
	}
	if code, out := r.guard(); code != 1 || !strings.Contains(out, "from 1.3.0 to 1.2.0") {
		t.Fatalf("entry 2, HEAD^1..HEAD: exit %d\n%s", code, out)
	}
	if code, _ := r.guard("-base", base); code != 0 {
		t.Fatalf("entry 2, base_sha..HEAD: exit %d (expected to miss it)", code)
	}
	// The other order passes both.
	r.git("checkout", "-q", "-b", "queue2", base)
	r.git("merge", "-q", "--no-ff", "--no-edit", "tool-1.2.0")
	if err := exec.Command("git", "-C", r.dir, "merge", "-q", "-X", "theirs", "--no-ff", "--no-edit", "tool-1.3.0").Run(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"-base", base}} {
		if code, out := r.guard(args...); code != 0 {
			t.Fatalf("1.2.0 then 1.3.0 %v: exit %d\n%s", args, code, out)
		}
	}
	// A push to main that lands a downgrade: before..after.
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--ff-only", "queue")
	if code, out := r.guard("-base", base, "-head", "main"); code != 0 {
		t.Fatalf("net push 1.1.0 -> 1.2.0: exit %d\n%s", code, out)
	}
	// -each checks every step of the push: the second merge goes down.
	if code, out := r.guard("-base", base, "-head", "main", "-each"); code != 1 || !strings.Contains(out, "from 1.3.0 to 1.2.0") {
		t.Fatalf("push, -each: exit %d\n%s", code, out)
	}
	if code, out := r.guard("-base", base, "-head", "queue2", "-each"); code != 0 {
		t.Fatalf("push 1.2.0 then 1.3.0, -each: exit %d\n%s", code, out)
	}
	if code, out := r.guard("-against", "main", "-each", "-head", "tool-1.2.0"); code != 2 || !strings.Contains(out, "excludes") {
		t.Fatalf("-against -each: exit %d\n%s", code, out)
	}
}
