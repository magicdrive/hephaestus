package hephaestus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// preflightRepo is a git repository with a hephaestus.yaml whose GitHub API
// is apiURL, and an origin that looks like GitHub.
func preflightRepo(t *testing.T, apiURL string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	cfg := `version: 1
project: {name: tool, description: d, homepage: "https://example.com", license: MIT}
archives:
  - name: "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}"
    targets: [{os: linux, arch: amd64}]
    files: [README.md]
release:
  github: {owner: acme, name: tool, api_url: "` + apiURL + `"}
`
	for name, content := range map[string]string{"hephaestus.yaml": cfg, "README.md": "hi\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "x"},
		{"remote", "add", "origin", "https://github.com/acme/tool.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestPreflightUsage(t *testing.T) {
	clearGitHubEnv(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"preflight"}, "--tag is required"},
		{[]string{"preflight", "--tag", "v1.0.0", "--format", "yaml"}, "--format must be text or json"},
		{[]string{"preflight", "--tag", "v1.0.0", "extra"}, "unexpected argument"},
		{[]string{"preflight", "--tag", "v1.0.0", "--offline", "--skip", "nope.check"}, "unknown check ID"},
	} {
		code, out, errs := runCLI(t, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errs, tc.want) {
			t.Errorf("%v: code %d out %q err %q", tc.args, code, out, errs)
		}
	}
	if code, _, errs := runCLI(t, "preflight", "-h"); code != 0 || !strings.Contains(errs, "git.repository, git.remote") {
		t.Errorf("help: %d %s", code, errs)
	}
}

// TestPreflightJSONOffline: --format json writes exactly one JSON document
// on stdout; local checks run, network checks are skipped.
func TestPreflightJSONOffline(t *testing.T) {
	clearGitHubEnv(t)
	chdir(t, preflightRepo(t, "https://api.github.com"))
	code, out, errs := runCLI(t, "preflight", "--tag", "v1.0.0", "--offline", "--branch", "main", "--format", "json")
	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		Status        string `json:"status"`
		Offline       bool   `json:"offline"`
		Checks        []struct {
			ID, Status, Message string
		} `json:"checks"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s\nstderr: %s", err, out, errs)
	}
	if dec.More() {
		t.Fatal("more than one JSON document on stdout")
	}
	if code != 0 || doc.Status != "pass" || !doc.Offline {
		t.Fatalf("code %d status %s\n%s\n%s", code, doc.Status, out, errs)
	}
	got := map[string]string{}
	for _, c := range doc.Checks {
		got[c.ID] = c.Status
	}
	for id, want := range map[string]string{"git.worktree": "PASS", "git.branch": "PASS", "tag.format": "PASS", "config.assets": "PASS", "github.repository": "SKIP", "tag.remote": "SKIP"} {
		if got[id] != want {
			t.Errorf("%s = %s, want %s", id, got[id], want)
		}
	}
	// --strict does not change a clean offline run, but a dirty tree fails.
	if err := os.WriteFile("untracked.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCLI(t, "preflight", "--tag", "v1.0.0", "--offline", "--branch", "main"); code != 1 || !strings.Contains(out, "FAIL    git.worktree") {
		t.Errorf("dirty: code %d\n%s", code, out)
	}
}

// TestPreflightNeverPrintsTheToken: even when GitHub echoes the token in an
// error, neither output format contains it, and every request is a GET.
func TestPreflightNeverPrintsTheToken(t *testing.T) {
	clearGitHubEnv(t)
	const token = "ghp_preflightSECRET0123456789"
	t.Setenv("GITHUB_TOKEN", token)
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"message":"bad credentials %s"}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	chdir(t, preflightRepo(t, srv.URL))
	for _, format := range []string{"text", "json"} {
		code, out, errs := runCLI(t, "preflight", "--tag", "v1.0.0", "--format", format, "--skip", "git.head.pushed,tag.remote,tag.order")
		if strings.Contains(out+errs, token) {
			t.Fatalf("%s output contains the token:\n%s\n%s", format, out, errs)
		}
		if code != 3 || !strings.Contains(out, "UNKNOWN") {
			t.Errorf("%s: code %d, want 3 (unknown)\n%s", format, code, out)
		}
		if code, _, _ := runCLI(t, "preflight", "--tag", "v1.0.0", "--format", format, "--strict", "--skip", "git.head.pushed,tag.remote,tag.order"); code != 1 {
			t.Errorf("%s strict: code %d, want 1", format, code)
		}
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("%s request", m)
		}
	}
	if len(methods) == 0 {
		t.Error("no request reached the server")
	}
}
