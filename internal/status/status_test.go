package status

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

type fakeGH struct {
	tag      bool
	releases []github.Release
	pulls    []github.PullRequest
	err      error
	calls    []string
}

func (f *fakeGH) GetRef(_ context.Context, _, _, ref string) (*github.Reference, error) {
	f.calls = append(f.calls, "GET ref "+ref)
	if f.err != nil {
		return nil, f.err
	}
	if !f.tag {
		return nil, github.ErrNotFound
	}
	r := &github.Reference{Ref: "refs/" + ref}
	r.Object.SHA = "aaaa"
	return r, nil
}
func (f *fakeGH) ListReleases(context.Context, string, string) ([]github.Release, error) {
	f.calls = append(f.calls, "GET releases")
	return f.releases, nil
}
func (f *fakeGH) ListPulls(context.Context, string, string, github.ListPullsOptions) ([]github.PullRequest, error) {
	f.calls = append(f.calls, "GET pulls")
	return f.pulls, nil
}

const sumA = "1111111111111111111111111111111111111111111111111111111111111111"

func digest(s string) *string { d := "sha256:" + s; return &d }

func manifestWith() *manifest.Manifest {
	m := manifest.New("tool", "1.0.0", "v1.0.0", "")
	m.Artifacts = []manifest.Artifact{
		{Kind: manifest.KindArchive, Name: "tool_1.0.0_linux_amd64.tar.gz", SHA256: sumA},
		{Kind: manifest.KindChecksum, Name: "tool_checksums.txt", SHA256: sumA},
	}
	return m
}

func asset(name, state string, d *string) github.Asset {
	return github.Asset{Name: name, State: state, Digest: d}
}

func complete() []github.Asset {
	return []github.Asset{asset("tool_1.0.0_linux_amd64.tar.gz", "uploaded", digest(sumA)), asset("tool_checksums.txt", "uploaded", digest(sumA))}
}

func input(gh *fakeGH, withTap bool) Input {
	cfg := &config.Config{Project: config.Project{Name: "tool"}}
	if withTap {
		cfg.Brews = []config.Brew{{Name: "tool", SkipUpload: "auto", Repository: config.BrewRepository{Owner: "acme", Name: "homebrew-tap"}}}
	}
	cfg.ApplyDefaults()
	return Input{Config: cfg, Owner: "acme", Repo: "tool", Tag: "v1.0.0", Manifest: manifestWith(), GitHub: gh,
		Vars: tmpl.Vars{Project: "tool", ProjectName: "tool", Version: "1.0.0", Tag: "v1.0.0", Env: map[string]string{}}}
}

func TestStates(t *testing.T) {
	merged := time.Now()
	for _, tc := range []struct {
		name  string
		gh    *fakeGH
		tap   bool
		state string
		next  string
	}{
		{"nothing", &fakeGH{}, false, NotStarted, "push the tag"},
		{"tag only", &fakeGH{tag: true}, false, Tagged, "never move or delete the tag"},
		{"draft, some assets", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Draft: true, Assets: complete()[:1]}}}, false, Draft, "re-run the release"},
		{"draft asset differs", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Draft: true, Assets: []github.Asset{asset("tool_1.0.0_linux_amd64.tar.gz", "uploaded", digest(strings.Repeat("2", 64)))}}}}, false, Conflict, "do not overwrite it"},
		{"published asset differs", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Assets: []github.Asset{asset("tool_1.0.0_linux_amd64.tar.gz", "uploaded", digest(strings.Repeat("2", 64))), complete()[1]}}}}, false, Conflict, "never replace it"},
		{"published, missing, mutable", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Assets: complete()[:1]}}}, false, PublishedIncomplete, "upload the missing assets"},
		{"published, missing, immutable", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Immutable: true, Assets: complete()[:1]}}}, false, Conflict, "release a new version"},
		{"starter asset", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Assets: []github.Asset{asset("tool_1.0.0_linux_amd64.tar.gz", "starter", nil), complete()[1]}}}}, false, PublishedIncomplete, "re-run"},
		{"complete, no tap", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Immutable: true, Assets: complete()}}}, false, Completed, "nothing to do"},
		{"tap pull request missing", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Assets: complete()}}}, true, Published, "--stages formula,tap"},
		{"tap pull request open", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Assets: complete()}}, pulls: []github.PullRequest{{State: "open", HTMLURL: "https://pr"}}}, true, Published, "review and merge"},
		{"tap pull request merged", &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0", Assets: complete()}}, pulls: []github.PullRequest{{State: "closed", MergedAt: &merged}}}, true, Completed, "nothing to do"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Run(context.Background(), input(tc.gh, tc.tap))
			if err != nil {
				t.Fatal(err)
			}
			if r.State != tc.state || !strings.Contains(r.Next, tc.next) {
				t.Errorf("state %s %q, want %s containing %q (assets %+v)", r.State, r.Next, tc.state, tc.next, r.Assets)
			}
			for _, c := range tc.gh.calls {
				if !strings.HasPrefix(c, "GET ") {
					t.Errorf("non-GET call %s", c)
				}
			}
		})
	}
}

func TestPrereleaseSkipsTapAndExtraAssets(t *testing.T) {
	gh := &fakeGH{tag: true, releases: []github.Release{{TagName: "v1.0.0-rc.1", Prerelease: true, Assets: append(complete(), asset("stray.txt", "uploaded", digest(sumA)))}}}
	in := input(gh, true)
	in.Tag, in.Vars.Prerelease = "v1.0.0-rc.1", "rc.1"
	r, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Completed || r.Taps[0].Status != "skipped" || r.Assets[0].Name != "stray.txt" || r.Assets[0].Status != "extra" {
		t.Errorf("%+v", r)
	}
	var b strings.Builder
	r.WriteText(&b)
	if !strings.Contains(b.String(), "State: COMPLETED") || !strings.Contains(b.String(), "extra      stray.txt") {
		t.Errorf("text:\n%s", b.String())
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Run(context.Background(), input(&fakeGH{err: errors.New("502")}, false)); err == nil {
		t.Error("a failed read must be an error, not a state")
	}
}
