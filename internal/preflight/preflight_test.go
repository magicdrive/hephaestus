package preflight

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/github"
)

// TestAllPass: a repository set up as recommended, releasing a stable
// version, passes every check (and the IDs are run in registry order).
func TestAllPass(t *testing.T) {
	f := newFixture(t, "v1.2.0")
	rep := f.run(t, true)
	for _, c := range rep.Checks {
		if c.Status != Pass {
			t.Errorf("%s: %s %s", c.ID, c.Status, c.Message)
		}
	}
	if rep.Status != OverallPass || rep.ExitCode() != ExitPass {
		t.Errorf("overall %s exit %d", rep.Status, rep.ExitCode())
	}
	var ids []string
	for _, c := range rep.Checks {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != strings.Join(Checks(), ",") {
		t.Errorf("order %v", ids)
	}
	if rep.Repository != "acme/tool" || rep.Commit != headSHA || rep.Version != "1.2.0" || *rep.Prerelease {
		t.Errorf("report header %+v", rep)
	}
}

func TestScenarios(t *testing.T) {
	cases := []struct {
		name   string
		tag    string
		setup  func(f *fixture)
		id     string
		status Status
		msg    string
	}{
		// Local git.
		{"outside a repository", "v1.2.0", func(f *fixture) { f.git.top = "" }, "git.repository", Fail, "not inside a git work tree"},
		{"dependent checks skipped outside a repository", "v1.2.0", func(f *fixture) { f.git.top = "" }, "git.worktree", Skip, "requires git.repository"},
		{"dirty", "v1.2.0", func(f *fixture) { f.git.status = " M main.go\n?? new.txt" }, "git.worktree", Fail, "2 paths are uncommitted"},
		{"detached", "v1.2.0", func(f *fixture) { f.git.branch = "" }, "git.branch", Fail, "detached"},
		{"wrong branch", "v1.2.0", func(f *fixture) { f.git.branch = "feature" }, "git.branch", Fail, "on feature, releases are tagged on main"},
		{"configured branch", "v1.2.0", func(f *fixture) {
			f.in.Branch = "release"
			f.git.branch = "release"
			f.git.refs["refs/heads/release"] = headSHA
		}, "git.branch", Pass, "release"},
		{"default branch unknown", "v1.2.0", func(f *fixture) { f.in.GitHub = nil; f.git.remoteHEAD = "" }, "git.branch", Unknown, "release branch unknown"},
		{"head not pushed", "v1.2.0", func(f *fixture) { f.git.refs["refs/heads/main"] = otherSHA }, "git.head.pushed", Fail, "differs from origin/main"},
		{"remote branch missing", "v1.2.0", func(f *fixture) { delete(f.git.refs, "refs/heads/main") }, "git.head.pushed", Fail, "has no branch main"},
		{"remote missing", "v1.2.0", func(f *fixture) { f.git.remotes = map[string]string{} }, "git.remote", Fail, "not configured"},
		{"remote is a fork", "v1.2.0", func(f *fixture) { f.git.remotes["origin"] = "https://github.com/someone/tool.git" }, "git.remote", Fail, "someone/tool"},
		{"ls-remote fails", "v1.2.0", func(f *fixture) { f.git.lsErr = errors.New("could not read from remote") }, "tag.remote", Unknown, "could not read from remote"},
		// Tag.
		{"invalid tag", "v1..0", nil, "tag.format", Fail, ""},
		{"non-SemVer tag", "release-7", nil, "tag.format", Warn, "not SemVer"},
		{"prerelease", "v1.2.0-rc.2", nil, "tag.format", Pass, "prerelease"},
		{"local tag exists", "v1.2.0", func(f *fixture) { f.git.localTags["v1.2.0"] = headSHA }, "tag.local", Fail, "already exists locally"},
		{"remote tag exists", "v1.1.0", nil, "tag.remote", Fail, "already exists on origin"},
		{"not newer", "v1.0.9", nil, "tag.order", Warn, "not newer than the released v1.1.0"},
		{"prerelease of a newer version", "v1.2.0-rc.2", nil, "tag.order", Pass, "newer than v1.1.0"},
		// Config.
		{"invalid config", "v1.2.0", func(f *fixture) { f.in.Config, f.in.ConfigErr = nil, errors.New("line 3: unknown key") }, "config.load", Fail, "unknown key"},
		{"config checks skipped", "v1.2.0", func(f *fixture) { f.in.Config, f.in.ConfigErr = nil, errors.New("bad") }, "config.assets", Skip, "requires config.load"},
		{"asset collision", "v1.2.0", func(f *fixture) {
			f.in.Config = loadConfig(t, strings.Replace(testConfig, `name: "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}"`, `name: "{{ .Project }}_{{ .Version }}_{{ .OS }}"`, 1))
		}, "config.assets", Fail, "collide"},
		{"checksum collides with an archive", "v1.2.0", func(f *fixture) {
			f.in.Config = loadConfig(t, strings.Replace(testConfig, `checksum: {name: "{{ .Project }}_checksums.txt"}`, `checksum: {name: "TOOL_1.2.0_LINUX_AMD64.tar.gz"}`, 1))
		}, "config.assets", Fail, "case-insensitively"},
		{"draft release", "v1.2.0", func(f *fixture) {
			f.in.Config = loadConfig(t, strings.Replace(testConfig, "notes_file: NOTES.md}", "notes_file: NOTES.md, draft: true}", 1))
		}, "config.release", Warn, "draft"},
		{"notes name the tag", "v1.2.0", nil, "config.release-notes", Pass, "NOTES.md name v1.2.0"},
		{"notes of the previous version", "v1.2.0", func(f *fixture) { writeNotes(t, f, "# tool v1.1.0\n") }, "config.release-notes", Fail, "does not mention v1.2.0 (versions found: v1.1.0)"},
		{"notes of the candidate, stable tag", "v1.2.0", func(f *fixture) { writeNotes(t, f, "# tool v1.2.0-rc.1\n") }, "config.release-notes", Fail, "does not mention v1.2.0"},
		{"notes of the stable, candidate tag", "v1.2.0-rc.1", func(f *fixture) { writeNotes(t, f, "# tool v1.2.0\n") }, "config.release-notes", Fail, "does not mention v1.2.0-rc.1"},
		{"notes with a longer version", "v1.2.0", func(f *fixture) { writeNotes(t, f, "see v1.2.01 and 11.2.0\n") }, "config.release-notes", Fail, "does not mention"},
		{"notes with the bare version", "v1.2.0", func(f *fixture) { writeNotes(t, f, "## 1.2.0 (2026-10-25)\n") }, "config.release-notes", Pass, "name v1.2.0"},
		{"notes missing", "v1.2.0", func(f *fixture) { os.Remove(filepath.Join(f.in.Config.Root, "NOTES.md")) }, "config.release-notes", Fail, "cannot be read"},
		{"notes empty", "v1.2.0", func(f *fixture) { writeNotes(t, f, " \n") }, "config.release-notes", Fail, "is empty"},
		{"changelog without the version", "v1.2.0", func(f *fixture) {
			f.in.Config.Preflight.Changelog = "CHANGELOG.md"
			os.WriteFile(filepath.Join(f.in.Config.Root, "CHANGELOG.md"), []byte("## v1.1.0\n"), 0o644)
		}, "config.release-notes", Fail, "CHANGELOG.md does not mention v1.2.0"},
		{"changelog with the version", "v1.2.0", func(f *fixture) {
			f.in.Config.Preflight.Changelog = "CHANGELOG.md"
			os.WriteFile(filepath.Join(f.in.Config.Root, "CHANGELOG.md"), []byte("## [1.2.0]\n"), 0o644)
		}, "config.release-notes", Pass, "NOTES.md, CHANGELOG.md"},
		{"notes outside the root", "v1.2.0", func(f *fixture) {
			f.in.Config.Preflight.Changelog = "../escape.md"
		}, "config.release-notes", Fail, "cannot be read"},
		{"prerelease tag published as stable", "v1.2.0-rc.1", func(f *fixture) { f.in.Config.Release.GitHub.Prerelease = "false" }, "config.release", Warn, "published as a stable release"},
		{"stable tag published as prerelease", "v1.2.0", func(f *fixture) { f.in.Config.Release.GitHub.Prerelease = "true" }, "config.release", Warn, "published as a prerelease"},
		{"no brews", "v1.2.0", func(f *fixture) {
			f.in.Config = loadConfig(t, testConfig[:strings.Index(testConfig, "brews:")]+"preflight: {environment: release}\n")
		}, "config.formula", Skip, "no Homebrew"},
		{"prerelease skips the tap", "v1.2.0-rc.1", nil, "config.formula", Pass, "tap skipped"},
		{"tap checks skipped for a prerelease", "v1.2.0-rc.1", nil, "tap.repository", Skip, "no formula is published"},
		{"no tap token needed for a prerelease", "v1.2.0-rc.1", nil, "github.secrets", Skip, "prerelease"},
		// GitHub.
		{"repository missing", "v1.2.0", func(f *fixture) { delete(f.gh.repos, "acme/tool") }, "github.repository", Fail, "not found"},
		{"repository archived", "v1.2.0", func(f *fixture) { f.gh.repos["acme/tool"].Archived = true }, "github.repository", Fail, "archived"},
		{"release exists", "v1.2.0", func(f *fixture) { f.gh.releases = []github.Release{{TagName: "v1.2.0", HTMLURL: "u"}} }, "github.release", Fail, "a published release"},
		{"draft exists", "v1.2.0", func(f *fixture) { f.gh.releases = []github.Release{{TagName: "v1.2.0", Draft: true}} }, "github.release", Fail, "a draft release"},
		{"drafts invisible without a token", "v1.2.0", func(f *fixture) { f.in.TokenPresent = false }, "github.release", Warn, "drafts are only listed"},
		{"ci failed", "v1.2.0", func(f *fixture) { f.gh.runs[1].Conclusion = "failure" }, "github.ci", Fail, "test (failure)"},
		{"ci pending", "v1.2.0", func(f *fixture) { f.gh.runs[1].Status = "in_progress" }, "github.ci", Fail, "still running"},
		{"required check missing", "v1.2.0", func(f *fixture) { f.gh.runs = f.gh.runs[1:] }, "github.ci", Fail, "ci-gate: not reported"},
		{"external status failed", "v1.2.0", func(f *fixture) {
			f.gh.status.TotalCount = 1
			f.gh.status.Statuses = append(f.gh.status.Statuses, struct {
				Context string `json:"context"`
				State   string `json:"state"`
			}{"ext/ci", "failure"})
		}, "github.ci", Fail, "ext/ci (failure)"},
		{"no ci at all", "v1.2.0", func(f *fixture) { f.gh.runs = nil; f.gh.rulesets = f.gh.rulesets[1:] }, "github.ci", Warn, "no CI results"},
		{"workflow missing", "v1.2.0", func(f *fixture) { f.gh.workflow = nil }, "github.workflow", Fail, "is not a workflow"},
		{"workflow disabled", "v1.2.0", func(f *fixture) { f.gh.workflow.State = "disabled_manually" }, "github.workflow", Fail, "disabled_manually"},
		{"no workflow configured", "v1.2.0", func(f *fixture) { f.in.Config.Preflight.Workflow = "" }, "github.workflow", Skip, "no release workflow configured"},
		{"environment missing", "v1.2.0", func(f *fixture) { f.gh.env = nil }, "github.environment", Fail, "does not exist"},
		{"environment without policy", "v1.2.0", func(f *fixture) { f.gh.env.DeploymentBranchPolicy = nil }, "github.environment", Fail, "any branch or tag"},
		{"environment on protected branches", "v1.2.0", func(f *fixture) { f.gh.env = envWith(false, true) }, "github.environment", Fail, "protected branches"},
		{"environment branch policy v* (the t mistake)", "v1.2.0", func(f *fixture) { f.gh.policies = []github.DeploymentPolicy{{Name: "v*", Type: "branch"}} }, "github.environment", Fail, "branches v*"},
		{"environment tag policy not matching", "v1.2.0", func(f *fixture) { f.gh.policies = []github.DeploymentPolicy{{Name: "release-*", Type: "tag"}} }, "github.environment", Fail, "no tag policy"},
		{"secret missing", "v1.2.0", func(f *fixture) { f.gh.envSecrets = nil }, "github.secrets", Fail, "HOMEBREW_TAP_GITHUB_TOKEN"},
		{"secret in the repository", "v1.2.0", func(f *fixture) { f.gh.envSecrets, f.gh.repoSecret = nil, []string{"HOMEBREW_TAP_GITHUB_TOKEN"} }, "github.secrets", Pass, "present"},
		{"extra required secret", "v1.2.0", func(f *fixture) { f.in.Secrets = []string{"SIGNING_KEY"} }, "github.secrets", Fail, "SIGNING_KEY"},
		{"secret found in an unconfigured environment", "v1.2.0", func(f *fixture) { f.in.Config.Preflight.Environment = "" }, "github.secrets", Pass, "present"},
		{"secret missing everywhere", "v1.2.0", func(f *fixture) { f.in.Config.Preflight.Environment, f.gh.envSecrets = "", nil }, "github.secrets", Fail, "missing secrets"},
		{"environments unreadable", "v1.2.0", func(f *fixture) {
			f.in.Config.Preflight.Environment = ""
			f.gh.errs["ListEnvironments"] = apiErr(403, "no")
		}, "github.secrets", Unknown, "environments"},
		{"secrets need admin", "v1.2.0", func(f *fixture) {
			f.gh.errs["ListEnvironmentSecretNames"] = apiErr(403, "Resource not accessible by integration")
		}, "github.secrets", Unknown, "403"},
		{"no branch ruleset", "v1.2.0", func(f *fixture) { f.gh.rulesets = f.gh.rulesets[1:] }, "github.ruleset.branch", Warn, "no active ruleset protects main"},
		{"no tag ruleset", "v1.2.0", func(f *fixture) { f.gh.rulesets = f.gh.rulesets[:1] }, "github.ruleset.tag", Warn, "no active ruleset"},
		{"tag ruleset disabled", "v1.2.0", func(f *fixture) { f.gh.rulesets[1].Enforcement = "disabled" }, "github.ruleset.tag", Warn, "no active ruleset"},
		{"tag ruleset blocks creation", "v1.2.0", func(f *fixture) {
			f.gh.rulesets[1] = rs("Lock", "tag", []string{"~ALL"}, "creation", "deletion", "update")
		}, "github.ruleset.tag", Fail, "forbids creating"},
		{"rulesets unreadable", "v1.2.0", func(f *fixture) { f.gh.errs["ListRulesets"] = apiErr(404, "Not Found") }, "github.ruleset.tag", Unknown, "rulesets"},
		{"rate limited", "v1.2.0", func(f *fixture) { f.gh.errs["ListReleases"] = apiErr(429, "rate limit") }, "github.release", Unknown, "429"},
		{"server error", "v1.2.0", func(f *fixture) { f.gh.errs["ListCheckRuns"] = apiErr(502, "bad gateway") }, "github.ci", Unknown, "502"},
		// Tap.
		{"tap missing", "v1.2.0", func(f *fixture) { delete(f.gh.repos, "acme/homebrew-tap") }, "tap.repository", Fail, "not found"},
		{"tap uses Formula/", "v1.2.0", func(f *fixture) { f.gh.tapDir = []github.DirEntry{{Name: "Formula", Type: "dir"}} }, "tap.layout", Fail, "Formula/"},
		{"tap branch exists", "v1.2.0", func(f *fixture) { f.gh.tapRefs["heads/tool-1.2.0"] = true }, "tap.branch", Fail, "already exists"},
		{"tap pull request open", "v1.2.0", func(f *fixture) { f.gh.pulls = []github.PullRequest{{Number: 7, State: "open"}} }, "tap.pull-request", Fail, "#7"},
		{"tap pull request closed", "v1.2.0", func(f *fixture) { f.gh.pulls = []github.PullRequest{{Number: 7, State: "closed"}} }, "tap.pull-request", Warn, "earlier pull request"},
		// Configuration and flags.
		{"skip by flag", "v1.2.0", func(f *fixture) { f.in.Skip = []string{"github.ruleset.branch"} }, "github.ruleset.branch", Skip, "skipped by configuration"},
		{"skip by config", "v1.2.0", func(f *fixture) { f.in.Config.Preflight.Skip = []string{"tap.layout"} }, "tap.layout", Skip, "skipped by configuration"},
		{"offline", "v1.2.0", func(f *fixture) { f.in.Offline, f.in.GitHub = true, nil }, "github.repository", Skip, "offline"},
		{"offline keeps local checks", "v1.2.0", func(f *fixture) { f.in.Offline, f.in.GitHub = true, nil }, "git.worktree", Pass, "clean"},
		{"repo override", "v1.2.0", func(f *fixture) { f.in.Repo = "other/tool" }, "git.remote", Fail, "releases go to other/tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.tag)
			if tc.setup != nil {
				tc.setup(f)
			}
			r := result(t, f.run(t, false), tc.id)
			if r.Status != tc.status || !strings.Contains(r.Message, tc.msg) {
				t.Errorf("%s = %s %q, want %s containing %q", tc.id, r.Status, r.Message, tc.status, tc.msg)
			}
			if (r.Status == Fail || r.Status == Warn) && r.Remediation == "" {
				t.Errorf("%s: %s without remediation", tc.id, r.Status)
			}
		})
	}
}

func apiErr(status int, msg string) error {
	return &github.APIError{StatusCode: status, Method: "GET", URL: "https://api.github.com/x", Message: msg}
}

func TestOverallAndExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *fixture)
		strict bool
		status string
		exit   int
	}{
		{"pass", nil, false, OverallPass, ExitPass},
		{"warn", func(f *fixture) { f.gh.rulesets = f.gh.rulesets[1:] }, false, OverallWarn, ExitPass},
		{"warn strict", func(f *fixture) { f.gh.rulesets = f.gh.rulesets[1:] }, true, OverallWarn, ExitFail},
		{"unknown", func(f *fixture) { f.gh.errs["ListRepositorySecretNames"] = apiErr(403, "forbidden") }, false, OverallUnknown, ExitIncomplete},
		{"unknown strict", func(f *fixture) { f.gh.errs["ListRepositorySecretNames"] = apiErr(403, "forbidden") }, true, OverallUnknown, ExitFail},
		{"fail beats unknown", func(f *fixture) {
			f.gh.errs["ListRepositorySecretNames"] = apiErr(403, "forbidden")
			f.git.status = "?? x"
		}, false, OverallFail, ExitFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "v1.2.0")
			if tc.setup != nil {
				tc.setup(f)
			}
			rep := f.run(t, tc.strict)
			if rep.Status != tc.status || rep.ExitCode() != tc.exit {
				t.Errorf("status %s exit %d, want %s %d", rep.Status, rep.ExitCode(), tc.status, tc.exit)
			}
		})
	}
}

func TestUnknownSkipIsAnError(t *testing.T) {
	f := newFixture(t, "v1.2.0")
	f.in.Skip = []string{"github.nope"}
	if _, err := Run(t.Context(), f.in, false, "test"); !errors.Is(err, ErrUnknownCheck) {
		t.Fatalf("err %v", err)
	}
}

// TestAPIReuse: each GitHub read happens once per run, however many checks
// use it.
func TestAPIReuse(t *testing.T) {
	f := newFixture(t, "v1.2.0")
	f.run(t, false)
	for m, n := range f.gh.calls {
		if n > 1 && !strings.HasPrefix(m, "GetRuleset") {
			t.Errorf("%s called %d times", m, n)
		}
	}
	if f.gh.calls["GetRuleset"] != 2 {
		t.Errorf("GetRuleset called %d times, want one per active ruleset", f.gh.calls["GetRuleset"])
	}
	lsRemote := 0
	for _, c := range f.git.calls {
		if c == "ls-remote" {
			lsRemote++
		}
	}
	if lsRemote != 1 {
		t.Errorf("git ls-remote ran %d times", lsRemote)
	}
}

// TestJSONIsStable: the same input gives byte-identical JSON, which decodes
// into the documented shape.
func TestJSONIsStable(t *testing.T) {
	render := func() []byte {
		f := newFixture(t, "v1.2.0")
		f.gh.rulesets = f.gh.rulesets[1:]
		var b bytes.Buffer
		if err := f.run(t, false).WriteJSON(&b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	a, b := render(), render()
	if !bytes.Equal(a, b) {
		t.Fatal("JSON differs between identical runs")
	}
	var doc struct {
		SchemaVersion int            `json:"schema_version"`
		Repository    string         `json:"repository"`
		Tag           string         `json:"tag"`
		Commit        string         `json:"commit"`
		Status        string         `json:"status"`
		Summary       map[string]int `json:"summary"`
		Checks        []struct {
			ID, Status, Severity, Message, Remediation string
			Evidence                                   map[string]any
		} `json:"checks"`
	}
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != SchemaVersion || doc.Repository != "acme/tool" || doc.Tag != "v1.2.0" || doc.Commit != headSHA ||
		doc.Status != OverallWarn || len(doc.Checks) != len(Checks()) || doc.Summary["WARN"] != 1 {
		t.Errorf("document %+v", doc)
	}
}

// TestRemoteURLCredentialsAreNotReported: a token embedded in a remote URL
// never reaches the report.
func TestRemoteURLCredentialsAreNotReported(t *testing.T) {
	f := newFixture(t, "v1.2.0")
	f.git.remotes["origin"] = "https://x-access-token:ghs_SECRETVALUE@github.com/acme/tool.git"
	var b bytes.Buffer
	rep := f.run(t, false)
	if err := rep.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	rep.WriteText(&b)
	if strings.Contains(b.String(), "SECRETVALUE") {
		t.Fatal("remote URL credentials in the report")
	}
	if r := result(t, rep, "git.remote"); r.Status != Pass {
		t.Errorf("git.remote %s %s", r.Status, r.Message)
	}
}

func TestTextOutput(t *testing.T) {
	f := newFixture(t, "v1.2.0-rc.1")
	f.git.status = "?? x"
	var b bytes.Buffer
	f.run(t, false).WriteText(&b)
	for _, want := range []string{"Release preflight: acme/tool v1.2.0-rc.1 (prerelease) at 111111111111", "FAIL    git.worktree", "→ commit, stash", "Result: FAIL", "Do not push v1.2.0-rc.1 yet."} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, b.String())
		}
	}
}

func ExampleChecks() {
	fmt.Println(strings.Join(Checks(), "\n"))
	// Output:
	// git.repository
	// git.remote
	// git.worktree
	// git.branch
	// git.head.pushed
	// tag.format
	// tag.local
	// tag.remote
	// tag.order
	// config.load
	// config.assets
	// config.release
	// config.release-notes
	// config.formula
	// github.repository
	// github.release
	// github.ci
	// github.workflow
	// github.environment
	// github.secrets
	// github.ruleset.branch
	// github.ruleset.tag
	// tap.repository
	// tap.layout
	// tap.branch
	// tap.pull-request
}

// TestEveryCheckIsDocumented: check IDs are part of the stable surface
// (docs/compatibility/policy.md); each one has a row in docs/preflight.md.
func TestEveryCheckIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "preflight.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range Checks() {
		if !strings.Contains(string(doc), "`"+id+"`") {
			t.Errorf("check %s is not documented in docs/preflight.md", id)
		}
	}
}
