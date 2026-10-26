package publish

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/preflight"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]bool{"v1.0.0": false, "v0.1.0-rc.1": true, "v2.0.0-beta.2": true, "v1.0.0-0": true} {
		v, err := ParseVersion(in)
		if err != nil || v.Tag != in || v.Prerelease != want {
			t.Errorf("ParseVersion(%q) = %+v, %v", in, v, err)
		}
	}
	for in, msg := range map[string]string{
		"":              "required",
		"1.0.0":         `did you mean v1.0.0`,
		"1.0.0-rc.1":    `did you mean v1.0.0-rc.1`,
		"release-1":     "not a release tag",
		"v1.0":          "not SemVer",
		"v1.0.0-rc.":    "not SemVer",
		"v01.0.0":       "not SemVer",
		"v1.0.0+build":  "build metadata",
		"v1.0.0-rc.01":  "leading zero",
		"v1.0.0;rm":     "must be 1-128",
		"v1.0.0 v2.0.0": "must be 1-128",
	} {
		if _, err := ParseVersion(in); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("ParseVersion(%q) = %v, want %q", in, err, msg)
		}
	}
}

func TestResolveArgs(t *testing.T) {
	ok := []struct {
		a    Args
		want string
	}{
		{Args{Positional: []string{"v1.0.0"}}, "v1.0.0"},
		{Args{VersionVar: "v1.0.0"}, "v1.0.0"},
		{Args{TagVar: "v1.0.0"}, "v1.0.0"},
	}
	for _, tc := range ok {
		if got, err := ResolveArgs(tc.a); err != nil || got != tc.want {
			t.Errorf("%+v: %q, %v", tc.a, got, err)
		}
	}
	for _, a := range []Args{
		{},
		{Positional: []string{"v1.0.0", "v2.0.0"}},
		{Positional: []string{"v1.0.0"}, VersionVar: "v1.0.0"},
		{VersionVar: "v1.0.0", TagVar: "v1.0.0"},
	} {
		if _, err := ResolveArgs(a); err == nil {
			t.Errorf("%+v accepted", a)
		}
	}
}

func TestParseGitHubRemote(t *testing.T) {
	for _, u := range []string{
		"https://github.com/magicdrive/hephaestus", "https://github.com/magicdrive/hephaestus.git", "https://github.com/magicdrive/hephaestus/",
		"git@github.com:magicdrive/hephaestus.git", "git@github.com:magicdrive/hephaestus", "ssh://git@github.com/magicdrive/hephaestus.git",
		"ssh://git@github.com:22/magicdrive/hephaestus", "https://user@github.com/magicdrive/hephaestus.git",
	} {
		r, err := ParseGitHubRemote(u)
		if err != nil || r != (Repo{"magicdrive", "hephaestus"}) {
			t.Errorf("%s: %v, %v", u, r, err)
		}
	}
	for _, u := range []string{"https://gitlab.com/magicdrive/hephaestus", "git@example.com:magicdrive/hephaestus.git", "/local/path", "file:///tmp/x", "https://github.com/magicdrive", "https://github.com/a/b/c"} {
		if r, err := ParseGitHubRemote(u); err == nil {
			t.Errorf("%s accepted as %v", u, r)
		}
	}
}

func TestParseLsRemoteTag(t *testing.T) {
	out := "aaaa\trefs/tags/v1.0.0\nbbbb\trefs/tags/v1.0.0^{}\n"
	if c, ok, _ := parseLsRemoteTag(out, "v1.0.0"); !ok || c != "bbbb" {
		t.Errorf("annotated: %s %t", c, ok)
	}
	if c, ok, _ := parseLsRemoteTag("aaaa\trefs/tags/v1.0.0\n", "v1.0.0"); !ok || c != "aaaa" {
		t.Errorf("lightweight: %s %t", c, ok)
	}
	if _, ok, _ := parseLsRemoteTag("aaaa\trefs/tags/v1.0.0-rc.1\n", "v1.0.0"); ok {
		t.Error("another tag matched")
	}
}

func TestPlan(t *testing.T) {
	p := testProject(t)
	rc := testPlan(t, p, "v0.1.0-rc.1")
	want := []string{
		"hephaestus_0.1.0-rc.1_darwin_amd64.tar.gz", "hephaestus_0.1.0-rc.1_darwin_arm64.tar.gz",
		"hephaestus_0.1.0-rc.1_linux_amd64.tar.gz", "hephaestus_0.1.0-rc.1_linux_arm64.tar.gz",
		"hephaestus_0.1.0-rc.1_windows_amd64.zip", "hephaestus_0.1.0-rc.1_windows_arm64.zip",
		"hephaestus_checksums.txt",
	}
	if got := rc.Assets(); !slices.Equal(got, want) {
		t.Errorf("assets %v", got)
	}
	if !rc.Prerelease || !rc.TapSkipped() || !strings.Contains(rc.Taps[0].Publish.SkipReason, "prerelease") {
		t.Errorf("rc plan %+v", rc)
	}
	st := testPlan(t, p, "v1.0.0")
	if st.Prerelease || st.TapSkipped() || !st.Taps[0].Publish.PullRequest || st.Taps[0].Publish.Path != "hephaestus.rb" ||
		st.Taps[0].URLPrefix != "https://github.com/magicdrive/hephaestus/releases/download/v1.0.0/" {
		t.Errorf("stable plan %+v", st.Taps)
	}
	for skip, wantSkip := range map[string]bool{"true": true, "false": false} {
		p.Cfg.Brews[0].SkipUpload = skip
		for _, tag := range []string{"v0.1.0-rc.1", "v1.0.0"} {
			if got := testPlan(t, p, tag).TapSkipped(); got != wantSkip {
				t.Errorf("skip_upload %s, %s: skipped %t", skip, tag, got)
			}
		}
	}
}

func TestRulesetAndEnvironmentChecks(t *testing.T) {
	gh := newFakeGH(t)
	if ok, d := tagRulesetProtects(gh.rulesets, "v1.0.0"); !ok {
		t.Error(d)
	}
	disabled := slices.Clone(gh.rulesets)
	disabled[1].Enforcement = "disabled"
	if ok, d := tagRulesetProtects(disabled, "v1.0.0"); ok || !strings.Contains(d, "no active tag ruleset") {
		t.Errorf("disabled ruleset passed: %s", d)
	}
	creation := []Ruleset{ruleset("t", "tag", "active", []string{"~ALL"}, "creation", "deletion", "update")}
	if ok, d := tagRulesetProtects(creation, "v1.0.0"); ok || !strings.Contains(d, "blocks creating") {
		t.Errorf("creation rule passed: %s", d)
	}
	excluded := []Ruleset{ruleset("t", "tag", "active", []string{"refs/tags/v*"}, "deletion", "update")}
	excluded[0].Conditions.RefName.Exclude = []string{"refs/tags/v1.*"}
	if ok, _ := tagRulesetProtects(excluded, "v1.0.0"); ok {
		t.Error("excluded tag passed")
	}
	if ok, d := branchRequiresCheck(gh.rulesets, "main", "main", "ci-gate"); !ok {
		t.Error(d)
	}
	if ok, _ := branchRequiresCheck(gh.rulesets, "main", "main", "other"); ok {
		t.Error("missing check passed")
	}
	def := []Ruleset{ruleset("d", "branch", "active", []string{"~DEFAULT_BRANCH"}, "required_status_checks")}
	if ok, _ := branchRequiresCheck(def, "main", "main", "ci-gate"); !ok {
		t.Error("~DEFAULT_BRANCH not understood")
	}
	// ~DEFAULT_BRANCH is the default branch, not the release branch.
	if ok, d := branchRequiresCheck(def, "release/stable", "develop", "ci-gate"); ok {
		t.Errorf("the default branch's ruleset protected release/stable: %s", d)
	}

	for _, tc := range []struct {
		env Environment
		ok  bool
	}{
		{gh.env, true},
		{Environment{Name: "release"}, false},
		{Environment{Name: "release", Protected: true}, false},
		{Environment{Name: "release", CustomPolicies: true, Policies: []EnvPolicy{{Name: "v*", Type: "tag"}, {Name: "main", Type: "branch"}}}, false},
		{Environment{Name: "release", CustomPolicies: true, Policies: []EnvPolicy{{Name: "release-*", Type: "tag"}}}, false},
	} {
		if ok, d := environmentAllowsOnlyTag(tc.env, "v1.0.0"); ok != tc.ok {
			t.Errorf("%+v: %t %s", tc.env, ok, d)
		}
	}

	if ok, _ := formulaDirMatches([]Content{{Name: "ark.rb", Type: "file"}}, "hephaestus.rb"); !ok {
		t.Error("root layout rejected")
	}
	if ok, _ := formulaDirMatches([]Content{{Name: "Formula", Type: "dir"}}, "hephaestus.rb"); ok {
		t.Error("root formula accepted although Homebrew reads Formula/")
	}
}

// publishRun runs Publish with the release workflow simulated.
func publishRun(t *testing.T, h *harness, tag string, opts Options) (*Outcome, *Plan) {
	t.Helper()
	p := testProject(t)
	pl := testPlan(t, p, tag)
	if h.git.onPush == nil {
		h.git.onPush = h.gh.startRelease(pl, h.d.Now)
	}
	h.gh.runPolls = 2
	return Publish(context.Background(), h.d, p, pl, opts), pl
}

func TestPublishPrerelease(t *testing.T) {
	h := newHarness(t)
	o, pl := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
	if o.State != Completed || o.ExitCode() != 0 {
		t.Fatalf("outcome %+v\n%s", o, h.out)
	}
	out := h.out.String()
	for _, want := range []string{
		"Commit:         " + testCommit, "Release type:   Prerelease", "Homebrew Tap:   SKIP", "Create and push this release tag? [y/N]",
		"[ok  ] attestations", "7 verified", "[ok  ] tap untouched", "[ok  ] tap steps", "tap token was not used", "[ok  ] latest",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if !slices.Equal(h.git.created, []string{"v0.1.0-rc.1"}) || !slices.Equal(h.git.pushed, []string{"v0.1.0-rc.1"}) {
		t.Errorf("created %v pushed %v", h.git.created, h.git.pushed)
	}
	if len(h.gh.attested) != len(pl.Assets()) {
		t.Errorf("attested %v", h.gh.attested)
	}
	if len(h.gh.tapPRs) != 0 {
		t.Error("a prerelease opened a tap pull request")
	}
}

func TestPublishStable(t *testing.T) {
	h := newHarness(t)
	o, _ := publishRun(t, h, "v1.0.0", fastOpts)
	if o.State != Completed {
		t.Fatalf("outcome %+v\n%s", o, h.out)
	}
	out := h.out.String()
	for _, want := range []string{"Release type:   Stable", "Homebrew Tap:   PUBLISH: pull request to magicdrive/homebrew-tap", "[ok  ] tap formula", "download urls and sha256 match", `[ok  ] tap completions              installs the configured completions (generate_completions_from_executable(bin/"hephaestus", "completion"))`, "[ok  ] tap pull request", "publish never merges"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	if !slices.ContainsFunc(o.Next, func(s string) bool { return strings.Contains(s, "review and merge the formula pull request") }) {
		t.Errorf("next steps %v", o.Next)
	}
}

// TestPublishRefusals: a failed preflight or a declined prompt creates and
// pushes nothing.
func TestPublishRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tag   string
		setup func(h *harness)
		want  string
	}{
		{"dirty worktree", "v0.1.0-rc.1", func(h *harness) { h.git.status = " M main.go" }, "[FAIL] worktree"},
		{"not main", "v0.1.0-rc.1", func(h *harness) { h.git.branch = "feature" }, "[FAIL] branch"},
		{"detached", "v0.1.0-rc.1", func(h *harness) { h.git.branch = "" }, "HEAD is detached"},
		{"remote mismatch", "v0.1.0-rc.1", func(h *harness) { h.git.origin = "https://github.com/someone/fork.git" }, "[FAIL] origin"},
		{"push url elsewhere", "v0.1.0-rc.1", func(h *harness) { h.git.pushURL = "git@github.com:someone/fork.git" }, "[FAIL] origin push url"},
		{"unpushed commit", "v0.1.0-rc.1", func(h *harness) { h.git.head = otherSHA }, "[FAIL] in sync with origin"},
		{"local tag exists", "v0.1.0-rc.1", func(h *harness) { h.git.localTags["v0.1.0-rc.1"] = testCommit }, "[FAIL] local tag"},
		{"remote tag exists", "v0.1.0-rc.1", func(h *harness) { h.git.remoteTags["v0.1.0-rc.1"] = testCommit }, "[FAIL] remote tag"},
		{"ci failed", "v0.1.0-rc.1", func(h *harness) { h.gh.ciRuns[0].Conclusion = "failure" }, "[FAIL] ci on commit"},
		{"ci running", "v0.1.0-rc.1", func(h *harness) { h.gh.ciRuns[0].Status = "in_progress" }, "wait for it"},
		{"ci green on another commit only", "v0.1.0-rc.1", func(h *harness) { h.gh.ciRuns[0].HeadSHA = otherSHA }, "no ci.yml push run"},
		{"ruleset disabled", "v0.1.0-rc.1", func(h *harness) { h.gh.rulesets[1].Enforcement = "disabled" }, "[FAIL] tag ruleset"},
		{"environment allows main", "v0.1.0-rc.1", func(h *harness) {
			h.gh.env.Policies = append(h.gh.env.Policies, EnvPolicy{Name: "main", Type: "branch"})
		}, "[FAIL] environment"},
		{"environment missing", "v0.1.0-rc.1", func(h *harness) { h.gh.envErr = ErrNotFound }, "[FAIL] environment"},
		{"github api failure", "v0.1.0-rc.1", func(h *harness) { h.gh.authErr = errors.New("gh: HTTP 502") }, "[FAIL] gh auth"},
		{"draft release left", "v0.1.0-rc.1", func(h *harness) {
			h.gh.releases = []Release{{TagName: "v0.1.0-rc.1", Draft: true, HTMLURL: "https://draft"}}
		}, "a draft release for v0.1.0-rc.1 already exists"},
		{"stable without tap secret", "v1.0.0", func(h *harness) { h.gh.secrets = nil }, "[FAIL] tap token secret"},
		{"stable with stale tap pull request", "v1.0.0", func(h *harness) {
			h.gh.tapPRs = []PullRequest{{Number: 1, State: "closed", HTMLURL: "https://tap/pull/1"}}
		}, "[FAIL] tap pull request"},
		{"declined", "v0.1.0-rc.1", func(h *harness) { h.d.In = strings.NewReader("\n") }, "not confirmed"},
		{"no terminal", "v0.1.0-rc.1", func(h *harness) { h.d.Interactive = false }, "confirmation needs a terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			o, _ := publishRun(t, h, tc.tag, fastOpts)
			if o.State != Failed || o.ExitCode() != 1 {
				t.Fatalf("outcome %+v", o)
			}
			if len(h.git.created) != 0 || len(h.git.pushed) != 0 {
				t.Errorf("created %v pushed %v", h.git.created, h.git.pushed)
			}
			if got := h.out.String() + o.Err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("want %q in\n%s", tc.want, got)
			}
		})
	}
}

func TestPublishYesSkipsPrompt(t *testing.T) {
	h := newHarness(t)
	h.d.Interactive, h.d.In = false, nil
	opts := fastOpts
	opts.Yes = true
	if o, _ := publishRun(t, h, "v0.1.0-rc.1", opts); o.State != Completed {
		t.Fatalf("outcome %+v\n%s", o, h.out)
	}
}

func TestPublishTagStage(t *testing.T) {
	t.Run("main moved while confirming", func(t *testing.T) {
		h := newHarness(t)
		h.git.moveMainOnCreate = true
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if o.State != Failed || o.FailedAt != TagCreatedLocally || len(h.git.pushed) != 0 {
			t.Fatalf("outcome %+v pushed %v", o, h.git.pushed)
		}
		if h.git.localTags["v0.1.0-rc.1"] == "" || !strings.Contains(strings.Join(o.Next, "\n"), "git tag -d v0.1.0-rc.1") {
			t.Errorf("the local tag must be left for the user to inspect: %v", o.Next)
		}
	})
	t.Run("push fails", func(t *testing.T) {
		h := newHarness(t)
		h.git.pushErr = errors.New("connection reset")
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if o.State != Failed || o.FailedAt != TagCreatedLocally || h.git.localTags["v0.1.0-rc.1"] == "" {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("interrupted push: outcome unknown, not unpublished", func(t *testing.T) {
		h := newHarness(t)
		h.git.pushErr, h.git.lostAfterPush = errors.New("signal: interrupt"), true
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if !o.MonitorLost || o.ExitCode() != 3 || strings.Contains(strings.Join(o.Next, "\n"), "nothing was published") {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("push succeeded but no remote tag", func(t *testing.T) {
		h := newHarness(t)
		h.git.pushVanishes = true
		if o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts); o.State != Failed || o.FailedAt != TagCreatedLocally {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("push reported an error but landed", func(t *testing.T) {
		h := newHarness(t)
		h.git.pushErr, h.git.pushLands = errors.New("remote hung up"), true
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if o.State != Completed || !strings.Contains(h.out.String(), "but origin has v0.1.0-rc.1") {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
}

func TestPublishWorkflowOutcomes(t *testing.T) {
	t.Run("run never appears", func(t *testing.T) {
		h := newHarness(t)
		h.git.onPush = func(string) {}
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if o.State != TagPushed || !o.MonitorLost || o.ExitCode() != 3 {
			t.Fatalf("outcome %+v", o)
		}
		if !strings.Contains(strings.Join(o.Next, "\n"), "make publish-watch v0.1.0-rc.1") {
			t.Errorf("next %v", o.Next)
		}
	})
	t.Run("a run of another commit is not followed", func(t *testing.T) {
		h := newHarness(t)
		h.git.onPush = func(tag string) {
			h.gh.runs = append(h.gh.runs, Run{ID: 5, Path: ".github/workflows/release.yml", Event: "push", HeadBranch: tag, HeadSHA: otherSHA, Status: "completed", Conclusion: "success", CreatedAt: h.now})
		}
		if o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts); !o.MonitorLost || o.Run != nil {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("an older run of the tag is not followed", func(t *testing.T) {
		h := newHarness(t)
		h.git.onPush = func(tag string) {
			h.gh.runs = append(h.gh.runs, Run{ID: 5, Path: ".github/workflows/release.yml", Event: "push", HeadBranch: tag, HeadSHA: testCommit, Status: "completed", Conclusion: "success", CreatedAt: h.now.Add(-time.Hour)})
		}
		if o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts); !o.MonitorLost || o.Run != nil {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("workflow fails", func(t *testing.T) {
		h := newHarness(t)
		h.gh.conclusion = "failure"
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if o.State != Failed || o.FailedAt != WorkflowDiscovered && o.FailedAt != WorkflowRunning || o.MonitorLost {
			t.Fatalf("outcome %+v", o)
		}
		if !strings.Contains(strings.Join(o.Next, "\n"), "nothing was published") {
			t.Errorf("next %v", o.Next)
		}
	})
	t.Run("workflow cancelled with a draft left", func(t *testing.T) {
		h := newHarness(t)
		h.gh.conclusion = "cancelled"
		h.gh.releases = nil
		p := testProject(t)
		pl := testPlan(t, p, "v0.1.0-rc.1")
		start := h.gh.startRelease(pl, h.d.Now)
		h.git.onPush = func(tag string) {
			start(tag)
			h.gh.releases = []Release{{TagName: tag, Draft: true}}
		}
		h.gh.runPolls = 2
		o := Publish(context.Background(), h.d, p, pl, fastOpts)
		if o.State != Failed || !strings.Contains(strings.Join(o.Next, "\n"), "re-running is safe") {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("monitoring loses the API", func(t *testing.T) {
		h := newHarness(t)
		h.gh.runByIDErr = errors.New("network down")
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if !o.MonitorLost || o.State != WorkflowDiscovered || o.Run == nil || !strings.Contains(strings.Join(o.Next, "\n"), "does not mean the release failed") {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("queued too long times out", func(t *testing.T) {
		h := newHarness(t)
		opts := fastOpts
		opts.QueuedWarning = 2 * time.Second
		opts.WatchTimeout = 5 * time.Second
		h.git.onPush = func(tag string) {
			h.gh.runs = append(h.gh.runs, Run{ID: 99, Path: ".github/workflows/release.yml", Event: "push", HeadBranch: tag, HeadSHA: testCommit, Status: "queued", CreatedAt: h.now})
		}
		h.gh.frozen = true
		o, _ := publishRun(t, h, "v0.1.0-rc.1", opts)
		if !o.MonitorLost || !strings.Contains(h.out.String(), "warning: run 99 has been") {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
}

// TestPublishVerificationInconclusive: a download that fails after a green
// run is a monitoring problem, not a failed release.
func TestPublishVerificationInconclusive(t *testing.T) {
	h := newHarness(t)
	h.gh.downloadErr = errors.New("connection reset")
	o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
	if !o.MonitorLost || o.ExitCode() != 3 || o.State != WorkflowSucceeded {
		t.Fatalf("outcome %+v\n%s", o, h.out)
	}
}

// TestPublishTapStepsLagging: right after the run, GitHub may report the
// release job's steps partially; the tap steps are read again instead of
// failing the verification (v1.0.0-rc.1 showed "found 0 of the 2 tap steps"
// for a correct release).
func TestPublishTapStepsLagging(t *testing.T) {
	partial := [][]Job{
		{{Name: "release", Status: "completed", Steps: []Step{{Name: "Plan the release"}}}},
		{{Name: "release", Status: "completed", Steps: []Step{{Name: TapPreflightStep}, {Name: TapPublishStep}}}},
	}
	t.Run("complete later", func(t *testing.T) {
		h := newHarness(t)
		h.gh.laggingJobs = partial
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if o.State != Completed || !strings.Contains(h.out.String(), "[ok  ] tap steps") || len(h.gh.laggingJobs) != 0 {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
	t.Run("never complete is inconclusive", func(t *testing.T) {
		h := newHarness(t)
		for range tapStepAttempts {
			h.gh.laggingJobs = append(h.gh.laggingJobs, partial[0])
		}
		o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
		if !o.MonitorLost || o.ExitCode() != 3 || !strings.Contains(h.out.String(), "step data incomplete") {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
}

func TestPublishVerificationFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tag   string
		after func(h *harness, pl *Plan)
		want  string
	}{
		{"asset missing", "v0.1.0-rc.1", func(h *harness, _ *Plan) {
			h.gh.releases[0].Assets = h.gh.releases[0].Assets[1:]
		}, "[FAIL] assets"},
		{"checksum mismatch", "v0.1.0-rc.1", func(h *harness, pl *Plan) {
			h.gh.assets[pl.Checksum] = []byte(strings.Repeat("0", 64) + "  " + pl.Archives[0] + "\n")
			h.gh.releases[0].Assets[len(h.gh.releases[0].Assets)-1].Digest = ""
		}, "[FAIL] checksums"},
		{"digest mismatch", "v0.1.0-rc.1", func(h *harness, pl *Plan) {
			h.gh.assets[pl.Archives[0]] = []byte("tampered")
		}, "[FAIL] asset digest"},
		{"attestation missing", "v0.1.0-rc.1", func(h *harness, pl *Plan) { h.gh.badAttest = map[string]bool{pl.Checksum: true} }, "[FAIL] attestations"},
		{"rc marked latest", "v0.1.0-rc.1", func(h *harness, _ *Plan) { h.gh.latest = "v0.1.0-rc.1" }, "[FAIL] latest"},
		{"rc not prerelease", "v0.1.0-rc.1", func(h *harness, _ *Plan) { h.gh.releases[0].Prerelease = false }, "[FAIL] prerelease flag"},
		{"rc tap moved", "v0.1.0-rc.1", func(h *harness, _ *Plan) { h.gh.tapHead = otherSHA }, "[FAIL] tap untouched"},
		{"rc tap pull request", "v0.1.0-rc.1", func(h *harness, _ *Plan) {
			h.gh.tapPRs = []PullRequest{{Number: 9, HTMLURL: "https://tap/pull/9"}}
		}, "[FAIL] tap untouched"},
		{"rc tap step ran", "v0.1.0-rc.1", func(h *harness, _ *Plan) { h.gh.jobs[0].Steps[1].Conclusion = "success" }, "[FAIL] tap steps"},
		{"stable without tap pull request", "v1.0.0", func(h *harness, _ *Plan) { h.gh.tapPRs = nil }, "[FAIL] tap pull request"},
		{"stable formula changes other files", "v1.0.0", func(h *harness, _ *Plan) { h.gh.prFiles = []string{"hephaestus.rb", "ark.rb"} }, "[FAIL] tap pull request"},
		{"stable formula wrong sha256", "v1.0.0", func(h *harness, _ *Plan) {
			h.gh.prFormula = "class X\n  url \"https://github.com/magicdrive/hephaestus/releases/download/v1.0.0/hephaestus_1.0.0_linux_amd64.tar.gz\"\n  sha256 \"" + strings.Repeat("0", 64) + "\"\nend\n"
		}, "[FAIL] tap formula"},
		{"stable formula foreign url", "v1.0.0", func(h *harness, _ *Plan) {
			h.gh.prFormula = "  url \"https://evil.example/x.tar.gz\"\n  sha256 \"" + strings.Repeat("0", 64) + "\"\n"
		}, "[FAIL] tap formula"},
		{"stable formula without completions", "v1.0.0", func(h *harness, _ *Plan) {
			h.gh.prFormula = strings.Replace(h.gh.prFormula, "generate_completions_from_executable", "# removed", 1)
		}, "[FAIL] tap completions"},
		{"stable formula with the completion line outside def install", "v1.0.0", func(h *harness, pl *Plan) {
			line := "    " + pl.Taps[0].Completions[0] + "\n"
			h.gh.prFormula = strings.Replace(h.gh.prFormula, line, "", 1)
			h.gh.prFormula = strings.Replace(h.gh.prFormula, "end\nend\n", "end\n\n  test do\n"+line+"  end\nend\n", 1)
		}, "[FAIL] tap completions"},
		{"stable not latest", "v1.0.0", func(h *harness, _ *Plan) { h.gh.latest = "v0.9.0" }, "[FAIL] latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := testProject(t)
			pl := testPlan(t, p, tc.tag)
			start := h.gh.startRelease(pl, h.d.Now)
			h.git.onPush = func(tag string) { start(tag); tc.after(h, pl) }
			h.gh.runPolls = 1
			o := Publish(context.Background(), h.d, p, pl, fastOpts)
			if o.State != Failed || o.FailedAt != WorkflowSucceeded {
				t.Fatalf("outcome %+v\n%s", o, h.out)
			}
			if !strings.Contains(h.out.String(), tc.want) {
				t.Errorf("want %q in\n%s", tc.want, h.out)
			}
		})
	}
}

func TestWatchAndStatus(t *testing.T) {
	h := newHarness(t)
	p := testProject(t)
	pl := testPlan(t, p, "v0.1.0-rc.1")
	ctx := context.Background()

	s, err := Observe(ctx, h.d, p, pl)
	if err != nil || s.State != NotStarted {
		t.Fatalf("before: %+v %v", s, err)
	}
	if o := Watch(ctx, h.d, p, pl, fastOpts); o.State != Failed || !strings.Contains(o.Err.Error(), "nothing to watch") {
		t.Fatalf("watch without a tag: %+v", o)
	}

	h.git.localTags["v0.1.0-rc.1"] = testCommit
	if s, _ := Observe(ctx, h.d, p, pl); s.State != TagCreatedLocally {
		t.Errorf("local only: %+v", s)
	}
	h.git.remoteTags["v0.1.0-rc.1"] = testCommit
	if s, _ := Observe(ctx, h.d, p, pl); s.State != TagPushed {
		t.Errorf("pushed: %+v", s)
	}
	h.gh.startRelease(pl, h.d.Now)("v0.1.0-rc.1")
	if s, _ := Observe(ctx, h.d, p, pl); s.State != WorkflowRunning {
		t.Errorf("running: %+v", s)
	}
	h.gh.runPolls = 1
	if o := Watch(ctx, h.d, p, pl, fastOpts); o.State != Completed {
		t.Fatalf("watch: %+v\n%s", o, h.out)
	}
	s, err = Observe(ctx, h.d, p, pl)
	if err != nil || s.State != Completed || s.Verify == nil || !s.Verify.OK() {
		t.Fatalf("after: %+v %v", s, err)
	}
	s.Print(h.d, pl)
	for _, want := range []string{"Remote tag:   " + testCommit, "Release run:  99 completed/success", "7 assets", "[ok  ] attestations", "State:        Completed"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("status lacks %q\n%s", want, h.out)
		}
	}
	if len(h.git.created)+len(h.git.pushed) != 0 {
		t.Error("watch or status changed tags")
	}

	h.git.localTags["v0.1.0-rc.1"] = otherSHA
	if s, _ := Observe(ctx, h.d, p, pl); s.State != Failed || !strings.Contains(s.Detail, "differs") {
		t.Errorf("diverged tag: %+v", s)
	}
}

func TestPublishLock(t *testing.T) {
	h := newHarness(t)
	unlock, err := lock(context.Background(), h.d)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
	if o.State != Failed || !strings.Contains(o.Err.Error(), "another publish is running") || len(h.git.created) != 0 {
		t.Fatalf("outcome %+v", o)
	}
	unlock()
	if o, _ := publishRun(t, newHarnessShared(t, h), "v0.1.0-rc.1", fastOpts); o.State != Completed {
		t.Fatalf("after unlock: %+v", o)
	}
}

// newHarnessShared is a fresh harness using h's lock directory.
func newHarnessShared(t *testing.T, h *harness) *harness {
	n := newHarness(t)
	n.git.dir = h.git.dir
	return n
}

// TestGenericPreflightGate: the generic strict preflight is an additional
// gate; its failure (or UNKNOWN) stops publish before anything is tagged.
func TestGenericPreflightGate(t *testing.T) {
	report := func(status string, checks ...preflight.Result) *preflight.Report {
		r := &preflight.Report{Status: status, Strict: true, Summary: map[preflight.Status]int{}, Checks: checks}
		for _, c := range checks {
			r.Summary[c.Status]++
		}
		return r
	}
	for _, tc := range []struct {
		name string
		rep  *preflight.Report
		err  error
		ok   bool
		want string
	}{
		{"pass", report(preflight.OverallPass, preflight.Result{ID: "git.worktree", Status: preflight.Pass}), nil, true, "generic preflight"},
		{"fail", report(preflight.OverallFail, preflight.Result{ID: "config.release-notes", Status: preflight.Fail, Message: "RELEASE_NOTES.md does not mention v0.1.0-rc.1"}), nil, false, "config.release-notes"},
		{"unknown is a failure in strict mode", report(preflight.OverallUnknown, preflight.Result{ID: "github.secrets", Status: preflight.Unknown, Message: "403"}), nil, false, "github.secrets"},
		{"error", nil, errors.New("gh auth token: not logged in"), false, "not logged in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var asked string
			h.d.Generic = func(_ context.Context, tag string) (*preflight.Report, error) { asked = tag; return tc.rep, tc.err }
			o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
			if asked != "v0.1.0-rc.1" {
				t.Errorf("generic preflight asked for %q", asked)
			}
			if (o.State == Completed) != tc.ok || !strings.Contains(h.out.String(), tc.want) {
				t.Fatalf("outcome %+v\n%s", o, h.out)
			}
			if !tc.ok && len(h.git.created)+len(h.git.pushed) != 0 {
				t.Error("a tag was created despite the generic preflight")
			}
		})
	}
}

func TestFormulaHasLines(t *testing.T) {
	line := `generate_completions_from_executable(bin/"x", "completion", shell_parameter_format: :cobra, shells: [:bash, :zsh])`
	wrapped := "class X < Formula\n  def install\n    bin.install \"x\"\n" +
		"    generate_completions_from_executable(bin/\"x\", \"completion\",\n" +
		"                                         shell_parameter_format: :cobra, shells: [:bash, :zsh])\n  end\nend\n"
	if ok, d := formulaHasLines(wrapped, []string{line}); !ok {
		t.Fatalf("wrapped call not found: %s", d)
	}
	for name, f := range map[string]string{
		"in test":    "class X < Formula\n  def install\n    bin.install \"x\"\n  end\n\n  test do\n    " + line + "\n  end\nend\n",
		"in comment": "class X < Formula\n  def install\n    # " + line + "\n  end\nend\n",
		"absent":     "class X < Formula\n  def install\n    bin.install \"x\"\n  end\nend\n",
	} {
		if ok, _ := formulaHasLines(f, []string{line}); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
