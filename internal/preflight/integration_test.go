package preflight

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
)

// git runs git in dir for test setup.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWithRemote makes a clone of a bare repository (the "remote") whose
// main branch has one commit and a tag v1.0.0. The clone's origin URL is
// rewritten to look like GitHub, with pushurl/insteadOf keeping it local.
func repoWithRemote(t *testing.T) (work, bare string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	gitT(t, root, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	gitT(t, root, "init", "--quiet", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "--quiet", "-m", "first")
	gitT(t, work, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
	gitT(t, work, "remote", "add", "origin", bare)
	gitT(t, work, "push", "--quiet", "origin", "main", "v1.0.0")
	gitT(t, work, "remote", "set-head", "origin", "main")
	return work, bare
}

// snapshot hashes every file under dir (path, mode, size, content), so any
// write to the repository is detected.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		line := fmt.Sprintf("%s %v %d", rel, info.Mode(), info.Size())
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			line += " " + hex.EncodeToString(sum[:]) + " " + info.ModTime().Format(time.RFC3339Nano)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestRealGitIsReadOnly runs the git checks against a real repository and
// its remote and proves that neither changed (no index refresh, no fetch,
// no ref update).
func TestRealGitIsReadOnly(t *testing.T) {
	work, bare := repoWithRemote(t)
	// A modified tracked file makes `git status` want to refresh the index.
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(work, "README.md"), future, future); err != nil {
		t.Fatal(err)
	}
	beforeWork, beforeBare := snapshot(t, filepath.Join(work, ".git")), snapshot(t, bare)

	in := Input{Tag: "v1.1.0", Git: GitCLI{Dir: work}, Repo: "acme/tool", Offline: false}
	in.Skip = []string{"git.remote"} // the bare remote is not a GitHub URL
	rep, err := Run(context.Background(), in, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]Status{
		"git.repository": Pass, "git.worktree": Pass, "git.branch": Pass, "git.head.pushed": Pass,
		"tag.format": Pass, "tag.local": Pass, "tag.remote": Pass, "tag.order": Pass,
	} {
		if r := result(t, rep, id); r.Status != want {
			t.Errorf("%s = %s %s", id, r.Status, r.Message)
		}
	}
	if snapshot(t, filepath.Join(work, ".git")) != beforeWork {
		t.Error("the preflight changed the repository's .git directory")
	}
	if snapshot(t, bare) != beforeBare {
		t.Error("the preflight changed the remote")
	}
}

func TestRealGitScenarios(t *testing.T) {
	work, bare := repoWithRemote(t)
	run := func(tag string) *Report {
		t.Helper()
		rep, err := Run(context.Background(), Input{Tag: tag, Git: GitCLI{Dir: work}, Repo: "acme/tool", Skip: []string{"git.remote"}}, false, "test")
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if r := result(t, run("v1.0.0"), "tag.remote"); r.Status != Fail {
		t.Errorf("existing remote tag: %s %s", r.Status, r.Message)
	}
	if r := result(t, run("v1.0.0"), "tag.local"); r.Status != Fail {
		t.Errorf("existing local tag: %s", r.Status)
	}
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := result(t, run("v1.1.0"), "git.worktree"); r.Status != Fail || !strings.Contains(r.Message, "1 path is") {
		t.Errorf("untracked file: %s %s", r.Status, r.Message)
	}
	gitT(t, work, "add", "new.txt")
	gitT(t, work, "commit", "--quiet", "-m", "local only")
	if r := result(t, run("v1.1.0"), "git.head.pushed"); r.Status != Fail || !strings.Contains(r.Message, "differs from origin/main") {
		t.Errorf("unpushed commit: %s %s", r.Status, r.Message)
	}
	gitT(t, work, "checkout", "--quiet", "--detach")
	if r := result(t, run("v1.1.0"), "git.branch"); r.Status != Fail || !strings.Contains(r.Message, "detached") {
		t.Errorf("detached: %s %s", r.Status, r.Message)
	}
	gitT(t, work, "checkout", "--quiet", "main")
	gitT(t, work, "remote", "set-url", "origin", filepath.Join(filepath.Dir(bare), "missing.git"))
	if r := result(t, run("v1.1.0"), "tag.remote"); r.Status != Unknown {
		t.Errorf("unreachable remote: %s %s", r.Status, r.Message)
	}
	gitT(t, work, "remote", "remove", "origin")
	rep, err := Run(context.Background(), Input{Tag: "v1.1.0", Git: GitCLI{Dir: work}, Repo: "acme/tool"}, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := result(t, rep, "git.remote"); r.Status != Fail || !strings.Contains(r.Message, "not configured") {
		t.Errorf("no remote: %s %s", r.Status, r.Message)
	}
	if r := result(t, rep, "git.branch"); r.Status != Unknown {
		t.Errorf("default branch without a remote: %s %s", r.Status, r.Message)
	}

	outside := t.TempDir()
	rep, err = Run(context.Background(), Input{Tag: "v1.1.0", Git: GitCLI{Dir: outside}, Repo: "acme/tool"}, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := result(t, rep, "git.repository"); r.Status != Fail {
		t.Errorf("outside a repository: %s %s", r.Status, r.Message)
	}
}

// TestGitCLIRefusesWrites: only the read-only subcommands run, always with
// --no-optional-locks.
func TestGitCLIRefusesWrites(t *testing.T) {
	var ran [][]string
	g := GitCLI{Exec: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		ran = append(ran, args)
		return nil, nil
	}}
	ctx := context.Background()
	for _, args := range [][]string{
		{"fetch", "origin"}, {"push", "origin", "v1"}, {"tag", "v1"}, {"remote", "add", "x", "y"}, {"remote", "set-url", "origin", "y"},
		{"symbolic-ref", "HEAD", "refs/heads/x"}, {"status"}, {"commit", "-m", "x"}, {"update-ref", "refs/heads/x", "HEAD"}, {},
	} {
		if _, err := g.run(ctx, args...); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("git %v was allowed (%v)", args, err)
		}
	}
	_, _ = g.TopLevel(ctx)
	_, _ = g.Head(ctx)
	_, _ = g.Branch(ctx)
	_, _ = g.Status(ctx)
	_, _ = g.RemoteURL(ctx, "origin", false)
	_, _ = g.RemoteURL(ctx, "origin", true)
	_, _ = g.LocalTag(ctx, "v1")
	_, _ = g.RemoteDefaultBranch(ctx, "origin")
	_, _ = g.LsRemote(ctx, "origin")
	if len(ran) != 9 {
		t.Fatalf("ran %d commands: %v", len(ran), ran)
	}
	for _, a := range ran {
		if a[0] != "--no-optional-locks" {
			t.Errorf("git %v without --no-optional-locks", a)
		}
	}
}

// fakeServer is a GitHub API on httptest that records every request.
type fakeServer struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	routes  map[string]func(w http.ResponseWriter, r *http.Request)
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.methods = append(s.methods, r.Method)
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()
	if h, ok := s.routes[r.URL.Path]; ok {
		h(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"message":"Not Found"}`)
}

func jsonRoute(v any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// newServer serves acme/tool configured as recommended.
func newServer(t *testing.T) (*fakeServer, *github.Client) {
	t.Helper()
	fs := &fakeServer{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	base := "/repos/acme/tool"
	tap := "/repos/acme/homebrew-tap"
	fs.routes = map[string]func(http.ResponseWriter, *http.Request){
		base:               jsonRoute(map[string]any{"full_name": "acme/tool", "default_branch": "main"}),
		base + "/releases": jsonRoute([]any{map[string]any{"tag_name": "v1.1.0"}}),
		base + "/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, r *http.Request) {
			// Two pages: the client must follow Link rel="next".
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []any{map[string]any{"name": "test", "status": "completed", "conclusion": "success"}}})
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []any{map[string]any{"name": "ci-gate", "status": "completed", "conclusion": "success"}}})
		},
		base + "/commits/" + headSHA + "/status":                  jsonRoute(map[string]any{"state": "pending", "total_count": 0}),
		base + "/actions/workflows/release.yml":                   jsonRoute(map[string]any{"path": ".github/workflows/release.yml", "state": "active"}),
		base + "/environments":                                    jsonRoute(map[string]any{"total_count": 1, "environments": []any{map[string]any{"name": "release"}}}),
		base + "/environments/release":                            jsonRoute(map[string]any{"name": "release", "deployment_branch_policy": map[string]any{"protected_branches": false, "custom_branch_policies": true}}),
		base + "/environments/release/deployment-branch-policies": jsonRoute(map[string]any{"total_count": 1, "branch_policies": []any{map[string]any{"name": "v*", "type": "tag"}}}),
		base + "/environments/release/secrets":                    jsonRoute(map[string]any{"total_count": 1, "secrets": []any{map[string]any{"name": "HOMEBREW_TAP_GITHUB_TOKEN"}}}),
		base + "/actions/secrets":                                 jsonRoute(map[string]any{"total_count": 0, "secrets": []any{}}),
		base + "/rulesets": jsonRoute([]any{
			map[string]any{"id": 1, "name": "Protect main", "target": "branch", "enforcement": "active"},
			map[string]any{"id": 2, "name": "Protect tags", "target": "tag", "enforcement": "active"},
		}),
		base + "/rulesets/1": jsonRoute(map[string]any{"id": 1, "name": "Protect main", "target": "branch", "enforcement": "active",
			"conditions": map[string]any{"ref_name": map[string]any{"include": []string{"~DEFAULT_BRANCH"}, "exclude": []string{}}},
			"rules":      []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []any{map[string]any{"context": "ci-gate"}}}}}}),
		base + "/rulesets/2": jsonRoute(map[string]any{"id": 2, "name": "Protect tags", "target": "tag", "enforcement": "active",
			"conditions": map[string]any{"ref_name": map[string]any{"include": []string{"refs/tags/v*"}, "exclude": []string{}}},
			"rules":      []any{map[string]any{"type": "deletion"}, map[string]any{"type": "update"}}}),
		tap:               jsonRoute(map[string]any{"full_name": "acme/homebrew-tap", "default_branch": "main"}),
		tap + "/contents": jsonRoute([]any{map[string]any{"name": "other.rb", "type": "file"}}),
		tap + "/pulls":    jsonRoute([]any{}),
	}
	c, err := github.New(github.Options{Token: "test-token-value", APIURL: srv.URL, Sleep: func(time.Duration) {}, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	return fs, c
}

// TestGitHubIsReadOnly runs every GitHub check against a recording server:
// every request is a GET, pagination is followed, and the result passes.
func TestGitHubIsReadOnly(t *testing.T) {
	srv, c := newServer(t)
	f := newFixture(t, "v1.2.0")
	f.in.GitHub = c
	rep := f.run(t, true)
	for _, r := range rep.Checks {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.ID, r.Status, r.Message)
		}
	}
	if r := result(t, rep, "github.ci"); !strings.Contains(r.Message, "2 checks") {
		t.Errorf("pagination not followed: %s", r.Message)
	}
	for i, m := range srv.methods {
		if m != http.MethodGet {
			t.Errorf("%s %s", m, srv.paths[i])
		}
	}
	if len(srv.methods) < 15 {
		t.Errorf("only %d requests: %v", len(srv.methods), srv.paths)
	}
	for _, p := range srv.paths {
		if strings.Contains(p, "/secrets/") {
			t.Errorf("a secret itself was requested: %s", p)
		}
	}
}

func TestGitHubErrorsAreUnknown(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv, c := newServer(t)
			srv.routes["/repos/acme/tool/releases"] = func(w http.ResponseWriter, _ *http.Request) {
				if code == 429 {
					w.Header().Set("Retry-After", "1")
				}
				w.WriteHeader(code)
				fmt.Fprint(w, `{"message":"nope"}`)
			}
			f := newFixture(t, "v1.2.0")
			f.in.GitHub = c
			r := result(t, f.run(t, false), "github.release")
			if r.Status != Unknown || !strings.Contains(r.Message, fmt.Sprint(code)) || r.Remediation == "" {
				t.Errorf("%d: %s %q %q", code, r.Status, r.Message, r.Remediation)
			}
		})
	}
}

// TestCheckRunsAcrossPages: the old success of a check on page 1 must not
// hide its newer failure on page 2, and every run is requested
// (filter=all), not the API's completed_at-based "latest".
func TestCheckRunsAcrossPages(t *testing.T) {
	srv, c := newServer(t)
	var queries []string
	srv.routes["/repos/acme/tool/commits/"+headSHA+"/check-runs"] = func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []any{
				map[string]any{"id": 2, "name": "ci-gate", "status": "completed", "conclusion": "failure", "app": map[string]any{"id": 15368, "slug": "github-actions"}}}})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?filter=all&page=2>; rel="next"`, r.Host, r.URL.Path))
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []any{
			map[string]any{"id": 1, "name": "ci-gate", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 15368, "slug": "github-actions"}}}})
	}
	f := newFixture(t, "v1.2.0")
	f.in.GitHub = c
	r := result(t, f.run(t, false), "github.ci")
	if r.Status != Fail || !strings.Contains(r.Message, "ci-gate [github-actions] (failure)") {
		t.Errorf("github.ci = %s %q", r.Status, r.Message)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "filter=all") {
		t.Errorf("queries %v", queries)
	}
}
