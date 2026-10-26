package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// onDefaultBranch makes the fakes a repository whose default branch is
// branch: the local clone on it, origin/HEAD naming it, its CI push run and
// a ruleset requiring ci-gate on it.
func onDefaultBranch(h *harness, branch string) {
	h.gh.defBranch = branch
	h.git.branch, h.git.originBranch, h.git.remoteHead = branch, branch, branch
	h.gh.ciRuns[0].HeadBranch = branch
	h.gh.rulesets[0].Conditions.RefName.Include = []string{"refs/heads/" + branch}
}

func preflightRun(t *testing.T, h *harness, p *Project, tag string) *Report {
	t.Helper()
	return Preflight(context.Background(), h.d, p, testPlan(t, p, tag))
}

func check(r *Report, name string) (Check, bool) {
	i := slices.IndexFunc(r.Checks, func(c Check) bool { return c.Name == name })
	if i < 0 {
		return Check{}, false
	}
	return r.Checks[i], true
}

// main, master and any other default branch: publish releases from the
// repository's default branch on GitHub, fetches that branch and nothing
// else, and requires the ruleset and the CI run on it.
func TestPreflight_ReleasesFromTheDefaultBranch(t *testing.T) {
	for _, branch := range []string{"main", "master", "trunk", "release/stable"} {
		t.Run(branch, func(t *testing.T) {
			h := newHarness(t)
			onDefaultBranch(h, branch)
			p := testProject(t)
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			if !r.OK() {
				var b strings.Builder
				r.Print(&b)
				t.Fatalf("preflight failed:\n%s", b.String())
			}
			if p.Branch != branch {
				t.Errorf("resolved %q, want %q", p.Branch, branch)
			}
			if c, _ := check(r, "default branch"); c.Detail != branch+" (GitHub, origin/HEAD)" {
				t.Errorf("default branch: %q", c.Detail)
			}
			if _, ok := check(r, branch+" ruleset"); !ok {
				t.Errorf("no %s ruleset check", branch)
			}
			if !slices.Equal(h.git.fetched, []string{branch}) {
				t.Errorf("fetched %v, want only %s", h.git.fetched, branch)
			}
		})
	}
}

// A whole release from master: the tag is created at the checked commit and
// pushed, after a re-fetch of origin/master.
func TestPublish_FromMaster(t *testing.T) {
	h := newHarness(t)
	onDefaultBranch(h, "master")
	o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
	if o.State != Completed {
		t.Fatalf("outcome %+v\n%s", o, h.out)
	}
	if !slices.Equal(h.git.pushed, []string{"v0.1.0-rc.1"}) || !slices.Equal(h.git.fetched, []string{"master", "master"}) {
		t.Errorf("pushed %v fetched %v", h.git.pushed, h.git.fetched)
	}
	if !strings.Contains(h.out.String(), "Branch:         master") {
		t.Errorf("the confirmation does not name master:\n%s", h.out)
	}
}

// How the branch is decided: an explicit -branch that names the default
// branch is checked; origin/HEAD may be unset.
func TestPreflight_DefaultBranchSources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(h *harness, p *Project)
		want     string
		resolved string
	}{
		{"explicit -branch master", func(h *harness, p *Project) { onDefaultBranch(h, "master"); p.Branch = "master" }, "master (GitHub, -branch, origin/HEAD)", "master"},
		{"origin/HEAD not set", func(h *harness, p *Project) { onDefaultBranch(h, "master"); h.git.remoteHead = "" }, "master (GitHub)", "master"},
		{"explicit -branch main, origin/HEAD not set", func(h *harness, p *Project) { h.git.remoteHead = ""; p.Branch = "main" }, "main (GitHub, -branch)", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := testProject(t)
			tc.setup(h, p)
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			if c, _ := check(r, "default branch"); !c.OK || c.Detail != tc.want || p.Branch != tc.resolved || !r.OK() {
				t.Errorf("default branch %+v, resolved %q, preflight ok %t", c, p.Branch, r.OK())
			}
		})
	}
}

// When the branch cannot be decided, or the sources disagree, the preflight
// fails closed at its first check: nothing that depends on the branch is
// checked or fetched, and publish neither creates nor pushes a tag.
func TestPublish_AmbiguousBranchTagsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness, p *Project)
		want  string
	}{
		{"-branch names another branch", func(h *harness, p *Project) { onDefaultBranch(h, "master"); p.Branch = "main" },
			"-branch main, but the default branch of magicdrive/hephaestus is master"},
		{"origin/HEAD names another branch (renamed default branch)", func(h *harness, p *Project) { onDefaultBranch(h, "main"); h.git.remoteHead = "master" },
			"refs/remotes/origin/HEAD names master, the default branch of magicdrive/hephaestus is main"},
		{"origin/HEAD cannot be read", func(h *harness, p *Project) { h.git.remoteHeadErr = errors.New("bad ref") },
			"refs/remotes/origin/HEAD: bad ref"},
		{"GitHub unreachable (offline)", func(h *harness, p *Project) { h.gh.repoErr = errors.New("dial tcp: no route to host") },
			"cannot read magicdrive/hephaestus's default branch, so nothing can be tagged"},
		{"no default branch reported", func(h *harness, p *Project) { h.gh.defBranch = "" },
			`reports the default branch "", not a branch name publish can use`},
		{"a default branch name that is no refspec", func(h *harness, p *Project) { h.gh.defBranch = "main:refs/heads/x" },
			`not a branch name publish can use`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := testProject(t)
			tc.setup(h, p)
			pl := testPlan(t, p, "v0.1.0-rc.1")
			h.git.onPush = h.gh.startRelease(pl, h.d.Now)
			o := Publish(context.Background(), h.d, p, pl, fastOpts)
			if o.State != Failed || o.FailedAt != NotStarted {
				t.Fatalf("outcome %+v\n%s", o, h.out)
			}
			if len(h.git.created)+len(h.git.pushed)+len(h.git.fetched) != 0 {
				t.Errorf("created %v pushed %v fetched %v", h.git.created, h.git.pushed, h.git.fetched)
			}
			out := h.out.String()
			if !strings.Contains(out, "[FAIL] default branch") || !strings.Contains(out, tc.want) {
				t.Errorf("want %q in\n%s", tc.want, out)
			}
			if strings.Contains(out, "] branch ") || strings.Contains(out, "in sync with origin") {
				t.Errorf("checked what depends on the branch:\n%s", out)
			}
		})
	}
}

// The local checkout must be the default branch: another branch, or a
// detached HEAD, fails (and is never tagged).
func TestPreflight_LocalBranchMustBeTheDefault(t *testing.T) {
	for name, local := range map[string]string{"on main, default master": "main", "detached HEAD": ""} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			onDefaultBranch(h, "master")
			h.git.branch = local
			p := testProject(t)
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			c, _ := check(r, "branch")
			if r.OK() || c.OK || !strings.Contains(c.Detail, "master") {
				t.Errorf("branch check %+v", c)
			}
		})
	}
}

// plan does not depend on the branch: it is computed from the configuration
// alone, with no branch resolved.
func TestPlan_NeedsNoBranch(t *testing.T) {
	p := testProject(t)
	if p.Branch != "" {
		t.Fatalf("a new project has branch %q before the preflight", p.Branch)
	}
	if pl := testPlan(t, p, "v1.2.3"); pl.Version.Tag != "v1.2.3" {
		t.Fatalf("plan %+v", pl)
	}
}

func TestValidBranchName(t *testing.T) {
	for _, name := range []string{"main", "master", "trunk", "release/1.x", "feat/a-b_c.d"} {
		if !validBranchName(name) {
			t.Errorf("%q rejected", name)
		}
	}
	for _, name := range []string{"", "HEAD", "-main", "+main", "a:b", "a*", "a b", "a..b", "a/", "/a", "a.lock", "a.", ".a", "a/.b", "a//b", "a@{1}", "a~1", "a^", "a?", "a[", `a\b`, "a\x7f", "a\tb"} {
		if validBranchName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}

// GitCLI against real repositories: origin/HEAD names the default branch
// (master, or any name) after a clone, also a shallow one; it is "" when it
// is unset and when there is no origin; FetchBranch updates exactly
// refs/remotes/origin/<branch> and refuses a name that is no branch.
func TestGitCLI_DefaultBranchOnRealRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// origin: a bare repository whose default branch is def, with one commit.
	origin := func(def string) string {
		root := t.TempDir()
		work := filepath.Join(root, "work")
		git(root, "init", "--quiet", "-b", def, work)
		git(work, "commit", "--quiet", "--allow-empty", "-m", "first")
		bare := filepath.Join(root, "origin.git")
		git(root, "clone", "--quiet", "--bare", work, bare)
		return bare
	}
	inDir := func(dir string) GitCLI {
		return GitCLI{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return ExecRunner(ctx, name, append([]string{"-C", dir}, args...)...)
		}}
	}

	for _, def := range []string{"master", "trunk"} {
		for _, shallow := range []bool{false, true} {
			name := def
			if shallow {
				name += " shallow"
			}
			t.Run(name, func(t *testing.T) {
				src := origin(def)
				clone := filepath.Join(t.TempDir(), "clone")
				args := []string{"clone", "--quiet"}
				if shallow {
					args = append(args, "--depth", "1", "file://"+src)
				} else {
					args = append(args, src)
				}
				git(filepath.Dir(clone), append(args, clone)...)
				g := inDir(clone)
				if b, err := g.RemoteHead(ctx); err != nil || b != def {
					t.Fatalf("RemoteHead %q %v, want %s", b, err, def)
				}
				if b, err := g.CurrentBranch(ctx); err != nil || b != def {
					t.Fatalf("CurrentBranch %q %v", b, err)
				}
				// a new commit on origin reaches refs/remotes/origin/<def> by FetchBranch
				work := filepath.Join(t.TempDir(), "w")
				git(filepath.Dir(work), "clone", "--quiet", src, work)
				git(work, "commit", "--quiet", "--allow-empty", "-m", "second")
				git(work, "push", "--quiet", "origin", "HEAD:refs/heads/"+def)
				want := git(work, "rev-parse", "HEAD")
				if err := g.FetchBranch(ctx, def); err != nil {
					t.Fatalf("FetchBranch: %v", err)
				}
				if c, err := g.Commit(ctx, "refs/remotes/origin/"+def); err != nil || c != want {
					t.Fatalf("origin/%s %q %v, want %s", def, c, err, want)
				}
				if tags := git(clone, "tag", "-l"); tags != "" {
					t.Errorf("FetchBranch fetched tags: %s", tags)
				}
				// detached HEAD
				git(clone, "checkout", "--quiet", "--detach")
				if b, err := g.CurrentBranch(ctx); err != nil || b != "" {
					t.Errorf("detached: %q %v", b, err)
				}
				// origin/HEAD unset
				git(clone, "remote", "set-head", "origin", "-d")
				if b, err := g.RemoteHead(ctx); err != nil || b != "" {
					t.Errorf("unset origin/HEAD: %q %v", b, err)
				}
			})
		}
	}

	t.Run("no origin", func(t *testing.T) {
		dir := t.TempDir()
		git(dir, "init", "--quiet", "-b", "master")
		g := inDir(dir)
		if b, err := g.RemoteHead(ctx); err != nil || b != "" {
			t.Errorf("RemoteHead without origin: %q %v", b, err)
		}
		if err := g.FetchBranch(ctx, "master"); err == nil {
			t.Error("FetchBranch without origin succeeded")
		}
	})
	t.Run("refspec-changing names never reach git", func(t *testing.T) {
		rec := &recorder{}
		g := GitCLI{Run: rec.run}
		for _, name := range []string{"main:refs/heads/x", "+main", "*", "-x"} {
			if err := g.FetchBranch(ctx, name); err == nil {
				t.Errorf("FetchBranch(%q) accepted", name)
			}
		}
		if len(rec.calls) != 0 {
			t.Errorf("git ran: %v", rec.calls)
		}
	})
}
