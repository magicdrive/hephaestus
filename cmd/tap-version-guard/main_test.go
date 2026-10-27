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
