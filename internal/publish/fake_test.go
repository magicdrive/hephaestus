package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
)

const (
	testCommit = "1111111111111111111111111111111111111111"
	otherSHA   = "2222222222222222222222222222222222222222"
	tapHead    = "3333333333333333333333333333333333333333"
)

// testConfig mirrors hephaestus.yaml in the parts publish reads.
const testConfig = `
version: 1
project: {name: hephaestus, description: d, homepage: "https://example.com", license: MIT}
builds:
  - id: hephaestus
    matrix: {os: [darwin, linux, windows], arch: [amd64, arm64]}
    command: [go, build, -o, "dist/bin/{{ .OS }}_{{ .Arch }}/hephaestus{{ .Ext }}", .]
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/hephaestus{{ .Ext }}"
archives:
  - id: hephaestus
    name: "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}"
    format: tar.gz
    format_overrides: [{os: windows, format: zip}]
checksum: {name: "{{ .Project }}_checksums.txt"}
release:
  github: {owner: magicdrive, name: hephaestus, generate_notes: true}
brews:
  - name: hephaestus
    skip_upload: auto
    repository: {owner: magicdrive, name: homebrew-tap, pull_request: {enabled: true}}
`

func testProject(t *testing.T) *Project {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(testConfig), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProject(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testPlan(t *testing.T, p *Project, tag string) *Plan {
	t.Helper()
	v, err := ParseVersion(tag)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := p.Plan(v)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// fakeGit is an in-memory clone of magicdrive/hephaestus on main.
type fakeGit struct {
	mu         sync.Mutex
	dir        string
	branch     string
	status     string
	origin     string
	pushURL    string
	head       string
	originMain string
	localTags  map[string]string
	remoteTags map[string]string
	fetchErr   error
	pushErr    error
	// lostAfterPush makes RemoteTag fail once the push was attempted.
	lostAfterPush bool
	// pushVanishes makes a "successful" push leave no remote tag.
	pushVanishes bool
	pushTried    bool
	// pushLands makes a failed push still create the remote tag.
	pushLands bool
	// onPush runs after a successful push (to start a fake workflow run).
	onPush func(tag string)
	// moveMainOnCreate moves origin/main when the tag is created (a push
	// to main while the prompt was open).
	moveMainOnCreate bool
	created, pushed  []string
}

func newFakeGit(t *testing.T) *fakeGit {
	return &fakeGit{
		dir: t.TempDir(), branch: "main", origin: "git@github.com:magicdrive/hephaestus.git",
		head: testCommit, originMain: testCommit, localTags: map[string]string{}, remoteTags: map[string]string{},
	}
}

func (g *fakeGit) TopLevel(context.Context) (string, error)  { return g.dir, nil }
func (g *fakeGit) CommonDir(context.Context) (string, error) { return g.dir, nil }
func (g *fakeGit) CurrentBranch(context.Context) (string, error) {
	return g.branch, nil
}
func (g *fakeGit) Status(context.Context) (string, error) { return g.status, nil }
func (g *fakeGit) RemoteURL(_ context.Context, _ string, push bool) (string, error) {
	if push && g.pushURL != "" {
		return g.pushURL, nil
	}
	return g.origin, nil
}
func (g *fakeGit) FetchMain(context.Context) error { return g.fetchErr }
func (g *fakeGit) Commit(_ context.Context, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case ref == "HEAD":
		return g.head, nil
	case ref == "refs/remotes/origin/main":
		return g.originMain, nil
	case strings.HasPrefix(ref, "refs/tags/"):
		return g.localTags[strings.TrimPrefix(ref, "refs/tags/")], nil
	}
	return "", nil
}
func (g *fakeGit) RemoteTag(_ context.Context, tag string) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lostAfterPush && g.pushTried {
		return "", false, errors.New("network down")
	}
	c, ok := g.remoteTags[tag]
	return c, ok, nil
}
func (g *fakeGit) CreateTag(_ context.Context, tag, commit, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.localTags[tag]; ok {
		return fmt.Errorf("tag %s exists", tag)
	}
	g.localTags[tag] = commit
	g.created = append(g.created, tag)
	if g.moveMainOnCreate {
		g.originMain = otherSHA
	}
	return nil
}
func (g *fakeGit) PushTag(_ context.Context, tag string) error {
	g.mu.Lock()
	g.pushTried = true
	if g.pushVanishes {
		g.mu.Unlock()
		return nil
	}
	if g.pushErr != nil {
		on := g.onPush
		if !g.pushLands {
			g.mu.Unlock()
			return g.pushErr
		}
		g.remoteTags[tag] = g.localTags[tag]
		g.mu.Unlock()
		if on != nil {
			on(tag)
		}
		return g.pushErr
	}
	if _, ok := g.remoteTags[tag]; ok {
		g.mu.Unlock()
		return fmt.Errorf("rejected: %s already exists", tag)
	}
	g.remoteTags[tag] = g.localTags[tag]
	g.pushed = append(g.pushed, tag)
	on := g.onPush
	g.mu.Unlock()
	if on != nil {
		on(tag)
	}
	return nil
}

// fakeGH is an in-memory GitHub: the repository's settings as configured
// in production, plus a release workflow that "runs" when a tag is pushed.
type fakeGH struct {
	mu          sync.Mutex
	t           *testing.T
	authErr     error
	ciRuns      []Run
	rulesets    []Ruleset
	env         Environment
	envErr      error
	secrets     []string
	releases    []Release
	latest      string
	runs        []Run
	runPolls    int  // RunByID calls before the run completes
	frozen      bool // RunByID never changes the run (stuck in the queue)
	conclusion  string
	runsErr     error // returned by Runs while non-nil
	runByIDErr  error
	tapHead     string
	tapPRs      []PullRequest
	tapFiles    map[string]string // path -> content on the tap's default branch
	prFiles     []string
	prFormula   string
	assets      map[string][]byte // name -> content served by DownloadRelease
	downloadErr error
	badAttest   map[string]bool
	attested    []string
	jobs        []Job
}

func newFakeGH(t *testing.T) *fakeGH {
	return &fakeGH{
		t:      t,
		ciRuns: []Run{{ID: 10, Path: ".github/workflows/ci.yml", Event: "push", HeadBranch: "main", HeadSHA: testCommit, Status: "completed", Conclusion: "success", HTMLURL: "https://ci/10", CreatedAt: time.Unix(100, 0)}},
		rulesets: []Ruleset{
			ruleset("Protect main", "branch", "active", []string{"refs/heads/main"}, "deletion", "non_fast_forward", "required_status_checks"),
			ruleset("Protect release tags", "tag", "active", []string{"refs/tags/v*"}, "deletion", "non_fast_forward", "update"),
		},
		env:        Environment{Name: "release", CustomPolicies: true, Policies: []EnvPolicy{{Name: "v*", Type: "tag"}}},
		secrets:    []string{"HOMEBREW_TAP_GITHUB_TOKEN"},
		conclusion: "success",
		tapHead:    tapHead,
		tapFiles:   map[string]string{"ark.rb": "class Ark < Formula\nend\n"},
	}
}

func ruleset(name, target, enforcement string, include []string, types ...string) Ruleset {
	rs := Ruleset{Name: name, Target: target, Enforcement: enforcement}
	rs.Conditions.RefName.Include = include
	for _, ty := range types {
		r := Rule{Type: ty}
		if ty == "required_status_checks" {
			r.Parameters.RequiredStatusChecks = append(r.Parameters.RequiredStatusChecks, struct {
				Context string `json:"context"`
			}{Context: "ci-gate"})
		}
		rs.Rules = append(rs.Rules, r)
	}
	return rs
}

// startRelease simulates the release workflow for a pushed tag: a run, and
// (when it succeeds) a published release with every asset.
func (f *fakeGH) startRelease(pl *Plan, now func() time.Time) func(string) {
	return func(tag string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.runs = append(f.runs, Run{ID: 99, Path: ".github/workflows/release.yml", Event: "push", HeadBranch: tag, HeadSHA: testCommit,
			Status: "queued", HTMLURL: "https://run/99", CreatedAt: now()})
		if f.assets == nil {
			f.assets = buildAssets(pl)
		}
		skip := "skipped"
		if !pl.TapSkipped() {
			skip = "success"
		}
		f.jobs = []Job{{Name: "release", Status: "completed", Conclusion: f.conclusion, Steps: []Step{
			{Name: TapPreflightStep, Conclusion: skip}, {Name: TapPublishStep, Conclusion: skip},
		}}}
		if f.conclusion != "success" {
			return
		}
		rel := Release{ID: 1, TagName: tag, Prerelease: pl.Prerelease, Body: "## What's Changed\n", HTMLURL: "https://release/" + tag}
		for _, name := range pl.Assets() {
			sum := sha256.Sum256(f.assets[name])
			rel.Assets = append(rel.Assets, Asset{Name: name, State: "uploaded", Size: int64(len(f.assets[name])), Digest: "sha256:" + hex.EncodeToString(sum[:])})
		}
		f.releases = append(f.releases, rel)
		if !pl.Prerelease {
			f.latest = tag
			f.tapPRs = []PullRequest{{Number: 7, State: "open", HTMLURL: "https://tap/pull/7"}}
			f.tapPRs[0].Head.Ref, f.tapPRs[0].Head.SHA = "hephaestus-"+pl.Version.Version, "4444444444444444444444444444444444444444"
			f.prFiles = []string{"hephaestus.rb"}
			f.prFormula = formulaFor(pl, rel)
		}
	}
}

func buildAssets(pl *Plan) map[string][]byte {
	out := map[string][]byte{}
	var sums bytes.Buffer
	for _, name := range pl.Archives {
		out[name] = []byte("archive " + name)
		s := sha256.Sum256(out[name])
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	out[pl.Checksum] = sums.Bytes()
	return out
}

func formulaFor(pl *Plan, rel Release) string {
	var b strings.Builder
	b.WriteString("class Hephaestus < Formula\n")
	for _, a := range rel.Assets {
		if a.Name == pl.Checksum || strings.Contains(a.Name, "windows") {
			continue
		}
		fmt.Fprintf(&b, "  url \"%s%s\"\n  sha256 \"%s\"\n", pl.Taps[0].URLPrefix, a.Name, strings.TrimPrefix(a.Digest, "sha256:"))
	}
	return b.String() + "end\n"
}

func (f *fakeGH) AuthStatus(context.Context) error { return f.authErr }
func (f *fakeGH) Repo(_ context.Context, r Repo) (RepoInfo, error) {
	return RepoInfo{FullName: r.String(), DefaultBranch: "main"}, nil
}
func (f *fakeGH) ActionsEnabled(context.Context, Repo) (bool, error) { return true, nil }
func (f *fakeGH) Workflow(_ context.Context, _ Repo, file string) (Workflow, error) {
	return Workflow{ID: 1, Path: ".github/workflows/" + file, State: "active"}, nil
}
func (f *fakeGH) Runs(_ context.Context, _ Repo, file, event, branch, sha string) ([]Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if file == "ci.yml" {
		return f.ciRuns, nil
	}
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	var out []Run
	for _, r := range f.runs {
		if r.HeadBranch == branch && r.HeadSHA == sha && r.Event == event {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeGH) RunByID(_ context.Context, _ Repo, id int64) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runByIDErr != nil {
		return Run{}, f.runByIDErr
	}
	for i := range f.runs {
		if f.runs[i].ID == id {
			r := &f.runs[i]
			if f.frozen {
				return *r, nil
			}
			f.runPolls--
			switch {
			case f.runPolls <= 0:
				r.Status, r.Conclusion = "completed", f.conclusion
			default:
				r.Status = "in_progress"
			}
			return *r, nil
		}
	}
	return Run{}, ErrNotFound
}
func (f *fakeGH) Jobs(context.Context, Repo, int64) ([]Job, error) { return f.jobs, nil }
func (f *fakeGH) Rulesets(context.Context, Repo) ([]Ruleset, error) {
	return f.rulesets, nil
}
func (f *fakeGH) Environment(context.Context, Repo, string) (Environment, error) {
	return f.env, f.envErr
}
func (f *fakeGH) EnvironmentSecrets(context.Context, Repo, string) ([]string, error) {
	return f.secrets, nil
}
func (f *fakeGH) Releases(context.Context, Repo) ([]Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Release(nil), f.releases...), nil
}
func (f *fakeGH) LatestRelease(context.Context, Repo) (Release, error) {
	if f.latest == "" {
		return Release{}, ErrNotFound
	}
	return Release{TagName: f.latest}, nil
}
func (f *fakeGH) Contents(context.Context, Repo, string, string) ([]Content, error) {
	var out []Content
	for name := range f.tapFiles {
		out = append(out, Content{Name: name, Type: "file"})
	}
	return out, nil
}
func (f *fakeGH) File(_ context.Context, _ Repo, path, ref string) ([]byte, error) {
	if ref != "main" && f.prFormula != "" {
		return []byte(f.prFormula), nil
	}
	if c, ok := f.tapFiles[path]; ok {
		return []byte(c), nil
	}
	return nil, ErrNotFound
}
func (f *fakeGH) BranchHead(_ context.Context, _ Repo, branch string) (string, error) {
	if branch == "main" {
		return f.tapHead, nil
	}
	return "", ErrNotFound
}
func (f *fakeGH) PullRequests(context.Context, Repo, string, string) ([]PullRequest, error) {
	return f.tapPRs, nil
}
func (f *fakeGH) PullRequestFiles(context.Context, Repo, int) ([]string, error) {
	return f.prFiles, nil
}
func (f *fakeGH) DownloadRelease(_ context.Context, _ Repo, _ string, dir string) error {
	if f.downloadErr != nil {
		return f.downloadErr
	}
	for name, data := range f.assets {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeGH) VerifyAttestation(_ context.Context, file string, p AttestPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.SignerWorkflow != "magicdrive/hephaestus/.github/workflows/release.yml" || p.SourceDigest != testCommit || !strings.HasPrefix(p.SourceRef, "refs/tags/v") {
		return fmt.Errorf("unexpected policy %+v", p)
	}
	name := filepath.Base(file)
	if f.badAttest[name] {
		return fmt.Errorf("%s: no attestation matched", name)
	}
	f.attested = append(f.attested, name)
	return nil
}

// harness wires fakes into Deps with a fake clock.
type harness struct {
	git *fakeGit
	gh  *fakeGH
	out *bytes.Buffer
	d   *Deps
	now time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{git: newFakeGit(t), gh: newFakeGH(t), out: &bytes.Buffer{}, now: time.Unix(1000, 0)}
	h.d = &Deps{
		Git: h.git, GH: h.gh, Out: h.out, In: strings.NewReader("y\n"), Interactive: true,
		Now:   func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error { h.now = h.now.Add(d); return ctx.Err() },
	}
	return h
}

var fastOpts = Options{Poll: time.Second, DiscoverTimeout: time.Minute, WatchTimeout: 10 * time.Minute, MaxAPIErrors: 3}
