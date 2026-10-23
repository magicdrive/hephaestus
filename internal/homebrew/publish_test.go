package homebrew

import (
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
)

func boolp(b bool) *bool { return &b }

func TestResolvePublishDefaults(t *testing.T) {
	b := baseBrew()
	b.Repository = config.BrewRepository{Owner: "acme", Name: "homebrew-tap", Token: "{{ .Env.TAP_TOKEN }}"}
	p, err := ResolvePublish(&b, testVars())
	if err != nil {
		t.Fatal(err)
	}
	want := Publish{
		Owner: "acme", Name: "homebrew-tap", Path: "mycli.rb",
		PullRequest: true, Branch: "mycli-1.2.3",
		Message:       "Brew formula update for mycli version v1.2.3",
		TokenTemplate: "{{ .Env.TAP_TOKEN }}",
	}
	if p != want {
		t.Errorf("got  %+v\nwant %+v", p, want)
	}
	tok, err := p.Token(testVars())
	if err != nil || tok != "secret" {
		t.Errorf("Token = %q, %v", tok, err)
	}
}

// TestTokenReadsHiddenCredentials checks that repository.token, unlike
// other templates, reads credentials that .Env hides.
func TestTokenReadsHiddenCredentials(t *testing.T) {
	v := testVars()
	v.Env = map[string]string{"PLAIN": "x"}
	v = v.WithSecretEnv(map[string]string{"HOMEBREW_TAP_GITHUB_TOKEN": " ghp_hidden "})
	p := Publish{TokenTemplate: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"}
	tok, err := p.Token(v)
	if err != nil || tok != "ghp_hidden" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	// Other values cannot read it.
	if _, err := ResolvePublish(&config.Brew{Name: "x", CommitMsg: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"}, v); err == nil {
		t.Fatal("commit_msg_template read a hidden credential")
	}
}

func TestResolvePublishDirect(t *testing.T) {
	b := baseBrew()
	b.Directory = "Formula"
	b.CommitAuthor = config.CommitAuthor{Name: "release-bot", Email: "bot@example.com"}
	b.CommitMsg = "{{ .ProjectName }} {{ .Version }}"
	b.Repository = config.BrewRepository{
		Owner: "acme", Name: "homebrew-tap", Branch: "main",
		PullRequest: &config.BrewPullRequest{Enabled: boolp(false)},
	}
	p, err := ResolvePublish(&b, testVars())
	if err != nil {
		t.Fatal(err)
	}
	if p.PullRequest || p.Branch != "main" || p.Path != "Formula/mycli.rb" || p.AuthorName != "release-bot" ||
		p.AuthorEmail != "bot@example.com" || p.Message != "mycli 1.2.3" {
		t.Errorf("got %+v", p)
	}
	// Direct mode without a branch means the default branch.
	b.Repository.Branch = ""
	if p, err := ResolvePublish(&b, testVars()); err != nil || p.Branch != "" {
		t.Errorf("got %+v, %v", p, err)
	}
}

func TestResolvePublishPullRequest(t *testing.T) {
	b := baseBrew()
	b.Repository = config.BrewRepository{
		Owner: "me", Name: "homebrew-tap", Branch: "update-{{ .Version }}",
		PullRequest: &config.BrewPullRequest{Enabled: boolp(true), Draft: true, Base: &config.BrewPRBase{Owner: "acme", Name: "homebrew-tap", Branch: "main"}},
	}
	p, err := ResolvePublish(&b, testVars())
	if err != nil {
		t.Fatal(err)
	}
	if !p.PullRequest || !p.Draft || p.Branch != "update-1.2.3" || p.BaseOwner != "acme" || p.BaseName != "homebrew-tap" || p.BaseBranch != "main" {
		t.Errorf("got %+v", p)
	}
}

func TestResolvePublishSkip(t *testing.T) {
	b := baseBrew()
	b.SkipUpload = "true"
	p, err := ResolvePublish(&b, testVars())
	if err != nil || !p.Skip {
		t.Errorf("skip_upload true: %+v, %v", p, err)
	}
	b.SkipUpload = "auto"
	v := testVars()
	if p, err := ResolvePublish(&b, v); err == nil || p.Skip {
		t.Errorf("auto on a release without repository must fail: %+v, %v", p, err)
	}
	v.Prerelease = "rc.1"
	if p, err := ResolvePublish(&b, v); err != nil || !p.Skip || !strings.Contains(p.SkipReason, "prerelease") {
		t.Errorf("auto on a prerelease: %+v, %v", p, err)
	}
}

func TestResolvePublishErrors(t *testing.T) {
	for _, tc := range []struct {
		mut func(b *config.Brew)
		err string
	}{
		{func(b *config.Brew) { b.Repository.Owner = "" }, "repository.owner is required"},
		{func(b *config.Brew) { b.Repository.Name = "bad name" }, "not a valid GitHub name"},
		{func(b *config.Brew) { b.Repository.Branch = "a..b" }, "not a valid branch name"},
		{func(b *config.Brew) { b.Repository.Branch = "-x" }, "not a valid branch name"},
		{func(b *config.Brew) { b.CommitAuthor = config.CommitAuthor{Name: "x"} }, "both name and email"},
		{func(b *config.Brew) { b.CommitAuthor = config.CommitAuthor{Name: "x", Email: "nope"} }, "not an email"},
		{func(b *config.Brew) { b.CommitMsg = "{{ .Nope }}" }, "commit_msg_template"},
		{func(b *config.Brew) { b.CommitMsg = "{{ \"\" }}" }, "empty message"},
		{func(b *config.Brew) { b.SkipUpload = "maybe" }, "skip_upload"},
		{func(b *config.Brew) { b.Repository.TokenType = "gitlab" }, "token_type"},
		{func(b *config.Brew) {
			b.Repository.PullRequest = &config.BrewPullRequest{Base: &config.BrewPRBase{Owner: "acme"}}
		}, "both owner and name"},
		{func(b *config.Brew) {
			b.Repository.Branch = "main"
			b.Repository.PullRequest = &config.BrewPullRequest{Base: &config.BrewPRBase{Branch: "main"}}
		}, "also the pull request base"},
	} {
		b := baseBrew()
		b.Repository = config.BrewRepository{Owner: "acme", Name: "homebrew-tap"}
		tc.mut(&b)
		if _, err := ResolvePublish(&b, testVars()); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("err = %v, want %q", err, tc.err)
		}
	}
}

func TestValidBranchName(t *testing.T) {
	for _, ok := range []string{"main", "feature/x", "mycli-1.2.3", "a_b"} {
		if !ValidBranchName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "@", "-a", "a/", "/a", "a..b", "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a.lock", "a/.b", "a@{b", "a.", "a//b", "a\x00"} {
		if ValidBranchName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
