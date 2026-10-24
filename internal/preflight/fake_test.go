package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
)

const (
	headSHA  = "1111111111111111111111111111111111111111"
	otherSHA = "2222222222222222222222222222222222222222"
)

// fakeGit is a repository on main, in sync with origin, clean.
type fakeGit struct {
	top, head, branch, status string
	remotes                   map[string]string // name -> URL
	localTags                 map[string]string
	refs                      map[string]string // ls-remote
	lsErr                     error
	remoteHEAD                string
	calls                     []string
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		top: "/repo", head: headSHA, branch: "main",
		remotes:   map[string]string{"origin": "git@github.com:acme/tool.git"},
		localTags: map[string]string{},
		refs: map[string]string{
			"refs/heads/main":       headSHA,
			"refs/tags/v1.1.0":      otherSHA,
			"refs/tags/v1.1.0^{}":   otherSHA,
			"refs/tags/v1.2.0-rc.1": otherSHA,
		},
		remoteHEAD: "main",
	}
}

func (g *fakeGit) rec(s string) { g.calls = append(g.calls, s) }
func (g *fakeGit) TopLevel(context.Context) (string, error) {
	g.rec("toplevel")
	if g.top == "" {
		return "", errors.New("fatal: not a git repository")
	}
	return g.top, nil
}
func (g *fakeGit) Head(context.Context) (string, error)   { g.rec("head"); return g.head, nil }
func (g *fakeGit) Branch(context.Context) (string, error) { g.rec("branch"); return g.branch, nil }
func (g *fakeGit) Status(context.Context) (string, error) { g.rec("status"); return g.status, nil }
func (g *fakeGit) RemoteURL(_ context.Context, r string, _ bool) (string, error) {
	g.rec("remote " + r)
	u, ok := g.remotes[r]
	if !ok {
		return "", fmt.Errorf("error: No such remote '%s'", r)
	}
	return u, nil
}
func (g *fakeGit) LocalTag(_ context.Context, t string) (string, error) {
	g.rec("localtag")
	return g.localTags[t], nil
}
func (g *fakeGit) RemoteDefaultBranch(context.Context, string) (string, error) {
	g.rec("remotehead")
	return g.remoteHEAD, nil
}
func (g *fakeGit) LsRemote(context.Context, string) (map[string]string, error) {
	g.rec("ls-remote")
	return g.refs, g.lsErr
}

// fakeGH is acme/tool on GitHub, configured as recommended, with tap
// acme/homebrew-tap.
type fakeGH struct {
	repos      map[string]*github.Repository
	releases   []github.Release
	runs       []github.CheckRun
	status     github.CombinedStatus
	workflow   *github.Workflow
	env        *github.Environment
	policies   []github.DeploymentPolicy
	envSecrets []string
	repoSecret []string
	rulesets   []github.Ruleset
	tapDir     []github.DirEntry
	tapRefs    map[string]bool
	pulls      []github.PullRequest
	errs       map[string]error // method -> error
	calls      map[string]int
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		repos: map[string]*github.Repository{
			"acme/tool":         {FullName: "acme/tool", DefaultBranch: "main"},
			"acme/homebrew-tap": {FullName: "acme/homebrew-tap", DefaultBranch: "main"},
		},
		runs:       []github.CheckRun{{Name: "ci-gate", Status: "completed", Conclusion: "success"}, {Name: "test", Status: "completed", Conclusion: "success"}},
		workflow:   &github.Workflow{Path: ".github/workflows/release.yml", State: "active"},
		env:        envWith(true, false),
		policies:   []github.DeploymentPolicy{{Name: "v*", Type: "tag"}},
		envSecrets: []string{"HOMEBREW_TAP_GITHUB_TOKEN"},
		rulesets:   []github.Ruleset{rs("Protect main", "branch", []string{"~DEFAULT_BRANCH"}, "deletion", "non_fast_forward", "required_status_checks"), rs("Protect tags", "tag", []string{"refs/tags/v*"}, "deletion", "update")},
		tapDir:     []github.DirEntry{{Name: "other.rb", Type: "file"}},
		tapRefs:    map[string]bool{},
		errs:       map[string]error{},
		calls:      map[string]int{},
	}
}

func envWith(custom, protected bool) *github.Environment {
	e := &github.Environment{Name: "release"}
	e.DeploymentBranchPolicy = &struct {
		ProtectedBranches    bool `json:"protected_branches"`
		CustomBranchPolicies bool `json:"custom_branch_policies"`
	}{protected, custom}
	return e
}

var rulesetIDs int64

func rs(name, target string, include []string, types ...string) github.Ruleset {
	rulesetIDs++
	r := github.Ruleset{ID: rulesetIDs, Name: name, Target: target, Enforcement: "active"}
	r.Conditions.RefName.Include = include
	for _, t := range types {
		rule := github.RulesetRule{Type: t}
		if t == "required_status_checks" {
			rule.Parameters.RequiredStatusChecks = append(rule.Parameters.RequiredStatusChecks, struct {
				Context       string `json:"context"`
				IntegrationID int64  `json:"integration_id"`
			}{Context: "ci-gate"})
		}
		r.Rules = append(r.Rules, rule)
	}
	return r
}

func (f *fakeGH) hit(m string) error { f.calls[m]++; return f.errs[m] }

func (f *fakeGH) GetRepository(_ context.Context, o, r string) (*github.Repository, error) {
	if err := f.hit("GetRepository " + o + "/" + r); err != nil {
		return nil, err
	}
	if repo, ok := f.repos[o+"/"+r]; ok {
		return repo, nil
	}
	return nil, fmt.Errorf("%w", github.ErrNotFound)
}
func (f *fakeGH) ListReleases(context.Context, string, string) ([]github.Release, error) {
	return f.releases, f.hit("ListReleases")
}
func (f *fakeGH) ListCheckRuns(context.Context, string, string, string) ([]github.CheckRun, error) {
	return f.runs, f.hit("ListCheckRuns")
}
func (f *fakeGH) GetCombinedStatus(context.Context, string, string, string) (*github.CombinedStatus, error) {
	return &f.status, f.hit("GetCombinedStatus")
}
func (f *fakeGH) GetWorkflow(context.Context, string, string, string) (*github.Workflow, error) {
	if err := f.hit("GetWorkflow"); err != nil {
		return nil, err
	}
	if f.workflow == nil {
		return nil, github.ErrNotFound
	}
	return f.workflow, nil
}
func (f *fakeGH) GetEnvironment(context.Context, string, string, string) (*github.Environment, error) {
	if err := f.hit("GetEnvironment"); err != nil {
		return nil, err
	}
	if f.env == nil {
		return nil, github.ErrNotFound
	}
	return f.env, nil
}
func (f *fakeGH) ListEnvironments(context.Context, string, string) ([]github.Environment, error) {
	if err := f.hit("ListEnvironments"); err != nil {
		return nil, err
	}
	if f.env == nil {
		return nil, nil
	}
	return []github.Environment{*f.env}, nil
}
func (f *fakeGH) ListDeploymentPolicies(context.Context, string, string, string) ([]github.DeploymentPolicy, error) {
	return f.policies, f.hit("ListDeploymentPolicies")
}
func (f *fakeGH) ListEnvironmentSecretNames(context.Context, string, string, string) ([]string, error) {
	return f.envSecrets, f.hit("ListEnvironmentSecretNames")
}
func (f *fakeGH) ListRepositorySecretNames(context.Context, string, string) ([]string, error) {
	return f.repoSecret, f.hit("ListRepositorySecretNames")
}
func (f *fakeGH) ListRulesets(context.Context, string, string) ([]github.Ruleset, error) {
	if err := f.hit("ListRulesets"); err != nil {
		return nil, err
	}
	var out []github.Ruleset
	for _, r := range f.rulesets {
		r.Rules = nil // the list endpoint omits rules
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeGH) GetRuleset(_ context.Context, _, _ string, id int64) (*github.Ruleset, error) {
	if err := f.hit("GetRuleset"); err != nil {
		return nil, err
	}
	for i := range f.rulesets {
		if f.rulesets[i].ID == id {
			return &f.rulesets[i], nil
		}
	}
	return nil, github.ErrNotFound
}
func (f *fakeGH) ListDirectory(context.Context, string, string, string, string) ([]github.DirEntry, error) {
	return f.tapDir, f.hit("ListDirectory")
}
func (f *fakeGH) GetRef(_ context.Context, _, _, ref string) (*github.Reference, error) {
	if err := f.hit("GetRef"); err != nil {
		return nil, err
	}
	if f.tapRefs[ref] {
		return &github.Reference{Ref: "refs/" + ref}, nil
	}
	return nil, github.ErrNotFound
}
func (f *fakeGH) ListPulls(context.Context, string, string, github.ListPullsOptions) ([]github.PullRequest, error) {
	return f.pulls, f.hit("ListPulls")
}

const testConfig = `
version: 1
project: {name: tool, description: d, homepage: "https://example.com", license: MIT}
builds:
  - id: tool
    matrix: {os: [darwin, linux], arch: [amd64, arm64]}
    command: [go, build, -o, "dist/{{ .OS }}_{{ .Arch }}/tool", .]
    output: "dist/{{ .OS }}_{{ .Arch }}/tool"
archives:
  - name: "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}"
checksum: {name: "{{ .Project }}_checksums.txt"}
release:
  github: {owner: acme, name: tool, notes_file: NOTES.md}
brews:
  - name: tool
    skip_upload: auto
    repository: {owner: acme, name: homebrew-tap}
preflight:
  workflow: release.yml
  environment: release
`

func loadConfig(t *testing.T, src string) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(src), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type fixture struct {
	git *fakeGit
	gh  *fakeGH
	in  Input
}

func newFixture(t *testing.T, tag string) *fixture {
	f := &fixture{git: newFakeGit(), gh: newFakeGH()}
	f.in = Input{Tag: tag, Config: loadConfig(t, testConfig), ConfigPath: "hephaestus.yaml", TokenPresent: true, Git: f.git, GitHub: f.gh}
	writeNotes(t, f, "# tool "+tag+"\n\nChanges.\n")
	return f
}

func (f *fixture) run(t *testing.T, strict bool) *Report {
	t.Helper()
	rep, err := Run(context.Background(), f.in, strict, "test")
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func result(t *testing.T, r *Report, id string) Result {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return Result{}
}

// writeNotes writes the fixture's release notes file (NOTES.md).
func writeNotes(t *testing.T, f *fixture, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.in.Config.Root, "NOTES.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
