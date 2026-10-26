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

// releaseFrom makes the fakes a repository whose default branch on GitHub
// is def and whose release branch is release: origin/HEAD names def, the
// local clone is on release at testCommit (= origin/release = release on
// GitHub), the CI push run is on release and a ruleset requires ci-gate on
// it. The default branch is at another commit.
func releaseFrom(h *harness, def, release string) {
	h.gh.defBranch = def
	h.gh.heads = map[string]string{def: otherSHA, release: testCommit}
	h.git.remoteHead = def
	h.git.branch, h.git.originBranch = release, release
	h.gh.ciRuns[0].HeadBranch = release
	h.gh.rulesets[0].Conditions.RefName.Include = []string{"refs/heads/" + release}
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

func printed(r *Report) string {
	var b strings.Builder
	r.Print(&b)
	return b.String()
}

// The release branch: -branch, else preflight.branch, else the default
// branch on GitHub, which is reported separately. Whichever it is, the
// preflight fetches only it, and requires its CI run and a ruleset on it.
func TestPreflight_ReleaseBranchResolution(t *testing.T) {
	for _, tc := range []struct {
		name, def, flag, configured string
		want, detail                string
	}{
		{"main", "main", "", "", "main", "main (the default branch)"},
		{"master", "master", "", "", "master", "master (the default branch)"},
		{"develop", "develop", "", "", "develop", "develop (the default branch)"},
		{"develop, preflight.branch release/stable", "develop", "", "release/stable", "release/stable", "release/stable (preflight.branch)"},
		{"main, -branch release/1.x", "main", "release/1.x", "", "release/1.x", "release/1.x (-branch)"},
		{"trunk, -branch production", "trunk", "production", "", "production", "production (-branch)"},
		{"-branch over preflight.branch", "develop", "release/stable", "release/1.x", "release/stable", "release/stable (-branch, over preflight.branch release/1.x)"},
		{"-branch naming the default branch", "master", "master", "", "master", "master (-branch, the default branch)"},
		{"preflight.branch naming the default branch", "main", "", "main", "main", "main (preflight.branch, the default branch)"},
		{"-branch and preflight.branch agree", "develop", "release/stable", "release/stable", "release/stable", "release/stable (-branch)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			releaseFrom(h, tc.def, tc.want)
			p := testProject(t)
			p.BranchFlag, p.Cfg.Preflight.Branch = tc.flag, tc.configured
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			if !r.OK() {
				t.Fatalf("preflight failed:\n%s", printed(r))
			}
			if p.Branch != tc.want || p.DefaultBranch != tc.def || r.Commit != testCommit || r.BranchHead != testCommit {
				t.Errorf("branch %q default %q commit %q head %q", p.Branch, p.DefaultBranch, r.Commit, r.BranchHead)
			}
			if c, _ := check(r, "default branch"); c.Detail != tc.def+" (GitHub, origin/HEAD)" {
				t.Errorf("default branch: %q", c.Detail)
			}
			if c, _ := check(r, "release branch"); c.Detail != tc.detail+" at "+short(testCommit)+" on GitHub" {
				t.Errorf("release branch: %q, want %q", c.Detail, tc.detail)
			}
			if c, _ := check(r, "repository"); !c.OK || !strings.Contains(c.Detail, "default branch "+tc.def) {
				t.Errorf("repository: %+v", c)
			}
			if c, ok := check(r, tc.want+" ruleset"); !ok || !c.OK {
				t.Errorf("no passing %s ruleset check: %+v", tc.want, c)
			}
			if c, _ := check(r, "ci on commit"); !c.OK {
				t.Errorf("ci on commit: %+v", c)
			}
			if !slices.Equal(h.git.fetched, []string{tc.want}) {
				t.Errorf("fetched %v, want only %s", h.git.fetched, tc.want)
			}
		})
	}
}

// A whole release from a release branch that is not the default branch,
// and one from master: the tag is created at the checked commit and pushed,
// after a re-fetch of the release branch; the confirmation names it.
func TestPublish_FromTheReleaseBranch(t *testing.T) {
	for _, tc := range []struct{ def, release string }{{"develop", "release/stable"}, {"master", "master"}} {
		t.Run(tc.release, func(t *testing.T) {
			h := newHarness(t)
			releaseFrom(h, tc.def, tc.release)
			p := testProject(t)
			p.Cfg.Preflight.Branch = tc.release
			pl := testPlan(t, p, "v0.1.0-rc.1")
			h.git.onPush = h.gh.startRelease(pl, h.d.Now)
			h.gh.runPolls = 2
			o := Publish(context.Background(), h.d, p, pl, fastOpts)
			if o.State != Completed {
				t.Fatalf("outcome %+v\n%s", o, h.out)
			}
			if !slices.Equal(h.git.pushed, []string{"v0.1.0-rc.1"}) || !slices.Equal(h.git.fetched, []string{tc.release, tc.release}) {
				t.Errorf("pushed %v fetched %v", h.git.pushed, h.git.fetched)
			}
			if !strings.Contains(h.out.String(), "Branch:         "+tc.release+"\n") {
				t.Errorf("the confirmation does not name %s:\n%s", tc.release, h.out)
			}
		})
	}
}

// When the release branch cannot be decided, the sources disagree or it is
// not a branch, the preflight fails closed at the branch checks: nothing
// that depends on the branch is checked or fetched, and publish neither
// creates nor pushes a tag.
func TestPublish_UnclearReleaseBranchTagsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness, p *Project)
		check string
		want  string
	}{
		{"origin/HEAD names another branch (renamed default branch)", func(h *harness, p *Project) { h.git.remoteHead = "master" },
			"default branch", "refs/remotes/origin/HEAD names master, the default branch of magicdrive/hephaestus is main"},
		{"origin/HEAD cannot be read", func(h *harness, p *Project) { h.git.remoteHeadErr = errors.New("bad ref") },
			"default branch", "refs/remotes/origin/HEAD: bad ref"},
		{"GitHub unreachable (offline)", func(h *harness, p *Project) { h.gh.repoErr = errors.New("dial tcp: no route to host") },
			"default branch", "cannot read magicdrive/hephaestus's default branch, so nothing can be tagged"},
		{"no default branch reported", func(h *harness, p *Project) { h.gh.defBranch = "" },
			"default branch", `reports the default branch "", not a branch name publish can use`},
		{"a default branch name that is no refspec", func(h *harness, p *Project) { h.gh.defBranch = "main:refs/heads/x" },
			"default branch", "not a branch name publish can use"},
		{"refspec injection in -branch", func(h *harness, p *Project) { p.BranchFlag = "main:refs/heads/x" },
			"release branch", `"main:refs/heads/x" (-branch) is not a branch name`},
		{"forced refspec in preflight.branch", func(h *harness, p *Project) { p.Cfg.Preflight.Branch = "+main" },
			"release branch", `"+main" (preflight.branch) is not a branch name`},
		{"a full ref", func(h *harness, p *Project) { p.BranchFlag = "refs/heads/main" },
			"release branch", `"refs/heads/main" (-branch) is not a branch name`},
		{"a tag", func(h *harness, p *Project) { p.BranchFlag = "v1.2.0" },
			"release branch", `"v1.2.0" (-branch) is not a branch name`},
		{"a commit", func(h *harness, p *Project) { p.BranchFlag = testCommit },
			"release branch", "(-branch) is not a branch name"},
		{"missing on GitHub", func(h *harness, p *Project) { p.BranchFlag = "release/stable" },
			"release branch", "release/stable (-branch): magicdrive/hephaestus has no branch release/stable"},
		{"its head cannot be read", func(h *harness, p *Project) { h.gh.headErr = errors.New("HTTP 502") },
			"release branch", "main (the default branch): cannot read its head on magicdrive/hephaestus, so nothing can be tagged: HTTP 502"},
		{"a head that is no commit", func(h *harness, p *Project) { h.gh.heads["main"] = "" },
			"release branch", `main (the default branch): magicdrive/hephaestus reports the head ""`},
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
			if !strings.Contains(out, "[FAIL] "+tc.check) || !strings.Contains(out, tc.want) {
				t.Errorf("want [FAIL] %s: %q in\n%s", tc.check, tc.want, out)
			}
			if strings.Contains(out, "] branch ") || strings.Contains(out, "in sync with origin") {
				t.Errorf("checked what depends on the branch:\n%s", out)
			}
		})
	}
}

// The local clone must be the release branch, exactly as it is on origin
// and on GitHub: another branch (also the default branch), a detached HEAD,
// unpushed commits or a release branch that moved fail the preflight.
func TestPreflight_CheckoutMustBeTheReleaseBranch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		check string
		want  string
	}{
		{"on the default branch", func(h *harness) { h.git.branch = "develop" }, "branch", "on develop; releases are cut from release/stable"},
		{"detached HEAD", func(h *harness) { h.git.branch = "" }, "branch", "HEAD is detached; switch to release/stable"},
		{"HEAD is not origin/release/stable", func(h *harness) { h.git.head = otherSHA }, "in sync with origin", "differs from origin/release/stable"},
		{"origin/release/stable is not GitHub's (moved, or a stale fetch)", func(h *harness) { h.git.originMain = otherSHA; h.git.head = otherSHA },
			"in sync with origin", "but release/stable on GitHub was " + short(testCommit)},
		{"the fetch fails", func(h *harness) { h.git.fetchErr = errors.New("couldn't find remote ref") }, "fetch origin/release/stable", "couldn't find remote ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			releaseFrom(h, "develop", "release/stable")
			tc.setup(h)
			p := testProject(t)
			p.BranchFlag = "release/stable"
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			c, _ := check(r, tc.check)
			unsynced := tc.check != "branch" // no commit is chosen to tag
			if r.OK() || c.OK || !strings.Contains(c.Detail, tc.want) || unsynced && r.Commit != "" {
				t.Errorf("%s: %+v (commit %q)\n%s", tc.check, c, r.Commit, printed(r))
			}
		})
	}
}

// The release branch moving on GitHub after the preflight (while the
// confirmation was open) stops publish before the push: the tag stays local.
func TestPublish_ReleaseBranchMovedBeforeThePush(t *testing.T) {
	h := newHarness(t)
	releaseFrom(h, "develop", "release/stable")
	h.gh.movesAfter = 1 // the preflight's read sees testCommit, the next otherSHA
	p := testProject(t)
	p.BranchFlag = "release/stable"
	pl := testPlan(t, p, "v0.1.0-rc.1")
	h.git.onPush = h.gh.startRelease(pl, h.d.Now)
	o := Publish(context.Background(), h.d, p, pl, fastOpts)
	if o.State != Failed || o.FailedAt != TagCreatedLocally || len(h.git.pushed) != 0 {
		t.Fatalf("outcome %+v pushed %v\n%s", o, h.git.pushed, h.out)
	}
	if !strings.Contains(o.Err.Error(), "release/stable on GitHub moved to \""+otherSHA) {
		t.Errorf("error: %v", o.Err)
	}
}

// A rejected tag push from a release branch fails with nothing on origin.
func TestPublish_PushRejectedFromTheReleaseBranch(t *testing.T) {
	h := newHarness(t)
	releaseFrom(h, "develop", "release/stable")
	h.git.pushErr = errors.New("! [remote rejected] v0.1.0-rc.1 (push declined due to repository rule violations)")
	p := testProject(t)
	p.BranchFlag = "release/stable"
	pl := testPlan(t, p, "v0.1.0-rc.1")
	o := Publish(context.Background(), h.d, p, pl, fastOpts)
	if o.State != Failed || o.FailedAt != TagCreatedLocally || len(h.git.remoteTags) != 0 || !strings.Contains(o.Err.Error(), "remote rejected") {
		t.Fatalf("outcome %+v remote tags %v\n%s", o, h.git.remoteTags, h.out)
	}
}

// CI must have succeeded on the release branch, on exactly the commit:
// a green run of the default branch does not count for the release branch.
func TestPreflight_CIOnTheReleaseBranch(t *testing.T) {
	green := func(branch string) Run {
		return Run{ID: 20, Path: ".github/workflows/ci.yml", Event: "push", HeadBranch: branch, HeadSHA: testCommit, Status: "completed", Conclusion: "success", HTMLURL: "https://ci/20"}
	}
	for _, tc := range []struct {
		name string
		runs func(h *harness) []Run
		ok   bool
		want string
	}{
		{"green on the release branch", func(h *harness) []Run { return h.gh.ciRuns }, true, "succeeded"},
		{"green on the default branch only", func(h *harness) []Run { return []Run{green("develop")} }, false, "no ci.yml push run on release/stable"},
		{"green on the default branch, red on the release branch", func(h *harness) []Run {
			red := h.gh.ciRuns[0]
			red.Conclusion = "failure"
			return []Run{green("develop"), red}
		}, false, "concluded failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			releaseFrom(h, "develop", "release/stable")
			h.gh.ciRuns = tc.runs(h)
			p := testProject(t)
			p.BranchFlag = "release/stable"
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			c, _ := check(r, "ci on commit")
			if c.OK != tc.ok || r.OK() != tc.ok || !strings.Contains(c.Detail, tc.want) {
				t.Errorf("ci on commit %+v, preflight ok %t", c, r.OK())
			}
		})
	}
}

// A ruleset must require ci-gate on the release branch itself. The default
// branch's ruleset (by name or ~DEFAULT_BRANCH) does not protect another
// release branch; a pattern or ~ALL does.
func TestPreflight_RulesetOfTheReleaseBranch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include []string
		checks  bool // the ruleset requires ci-gate
		active  bool
		ok      bool
	}{
		{"protected release branch", []string{"refs/heads/release/stable"}, true, true, true},
		{"a pattern covering it", []string{"refs/heads/release/*"}, true, true, true},
		{"~ALL", []string{"~ALL"}, true, true, true},
		{"only the default branch, by name", []string{"refs/heads/develop"}, true, true, false},
		{"only ~DEFAULT_BRANCH", []string{"~DEFAULT_BRANCH"}, true, true, false},
		{"no applicable ruleset", []string{"refs/heads/other"}, true, true, false},
		{"no required ci-gate", []string{"refs/heads/release/stable"}, false, true, false},
		{"not active", []string{"refs/heads/release/stable"}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			releaseFrom(h, "develop", "release/stable")
			rs := &h.gh.rulesets[0]
			rs.Conditions.RefName.Include = tc.include
			if !tc.checks {
				rs.Rules = rs.Rules[:2] // deletion, non_fast_forward
			}
			if !tc.active {
				rs.Enforcement = "evaluate"
			}
			p := testProject(t)
			p.BranchFlag = "release/stable"
			r := preflightRun(t, h, p, "v0.1.0-rc.1")
			c, _ := check(r, "release/stable ruleset")
			if c.OK != tc.ok || r.OK() != tc.ok {
				t.Errorf("ruleset %+v, preflight ok %t\n%s", c, r.OK(), printed(r))
			}
		})
	}
}

// plan does not depend on the branch: it is computed from the configuration
// alone, with no branch resolved.
func TestPlan_NeedsNoBranch(t *testing.T) {
	p := testProject(t)
	if p.Branch != "" || p.DefaultBranch != "" {
		t.Fatalf("a new project has branch %q (default %q) before the preflight", p.Branch, p.DefaultBranch)
	}
	if pl := testPlan(t, p, "v1.2.3"); pl.Version.Tag != "v1.2.3" {
		t.Fatalf("plan %+v", pl)
	}
}

func TestReleaseBranchName(t *testing.T) {
	for _, name := range []string{"main", "release/stable", "release/1.x", "production", "1.x"} {
		if !releaseBranchName(name) {
			t.Errorf("%q rejected", name)
		}
	}
	for _, name := range []string{"", "a:b", "refs/heads/main", "v1.2.3", "v0.1.0-rc.1", testCommit, strings.Repeat("ab", 32)} {
		if releaseBranchName(name) {
			t.Errorf("%q accepted", name)
		}
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
// refs/remotes/origin/<branch>, also of a release branch that is not the
// default branch, fails for a branch origin does not have and refuses a
// name that is no branch.
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
				// a release branch that is not the default branch, also in a
				// shallow clone (whose refspec names only the default branch)
				git(work, "push", "--quiet", "origin", "HEAD:refs/heads/release/stable")
				if err := g.FetchBranch(ctx, "release/stable"); err != nil {
					t.Fatalf("FetchBranch release/stable: %v", err)
				}
				if c, err := g.Commit(ctx, "refs/remotes/origin/release/stable"); err != nil || c != want {
					t.Fatalf("origin/release/stable %q %v, want %s", c, err, want)
				}
				if b, err := g.RemoteHead(ctx); err != nil || b != def {
					t.Errorf("RemoteHead after fetching release/stable: %q %v, want %s", b, err, def)
				}
				// a release branch origin does not have
				if err := g.FetchBranch(ctx, "release/missing"); err == nil {
					t.Error("FetchBranch of a missing branch succeeded")
				}
				if c, err := g.Commit(ctx, "refs/remotes/origin/release/missing"); err != nil || c != "" {
					t.Errorf("origin/release/missing %q %v", c, err)
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

// GHCLI.BranchHead asks for exactly refs/heads/<branch> (a slash in the name
// is escaped) and accepts only an answer for that ref.
func TestGHCLI_BranchHeadIsExact(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, reply string
		want        string
		ok          bool
	}{
		{"the branch", `{"ref":"refs/heads/release/stable","object":{"sha":"` + testCommit + `"}}`, testCommit, true},
		{"another ref", `{"ref":"refs/heads/release/stable-2","object":{"sha":"` + testCommit + `"}}`, "", false},
		{"no ref", `{"object":{"sha":"` + testCommit + `"}}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{reply: func(string, []string) ([]byte, error) { return []byte(tc.reply), nil }}
			sha, err := GHCLI{Run: rec.run}.BranchHead(ctx, Repo{Owner: "magicdrive", Name: "hephaestus"}, "release/stable")
			if (err == nil) != tc.ok || sha != tc.want {
				t.Errorf("BranchHead = %q, %v", sha, err)
			}
			if len(rec.calls) != 1 || !strings.HasSuffix(strings.Join(rec.calls[0], " "), "repos/magicdrive/hephaestus/git/ref/heads/release%2Fstable") {
				t.Errorf("calls %v", rec.calls)
			}
		})
	}
}
