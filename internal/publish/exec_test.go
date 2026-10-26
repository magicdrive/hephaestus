package publish

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// recorder is a Runner that records calls and answers from a table.
type recorder struct {
	calls [][]string
	reply func(name string, args []string) ([]byte, error)
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.reply != nil {
		return r.reply(name, args)
	}
	return []byte("{}"), nil
}

// TestGHCLIIsReadOnly calls every GitHub method and checks that each gh
// invocation is a GET API call or one of the read-only subcommands.
func TestGHCLIIsReadOnly(t *testing.T) {
	rec := &recorder{reply: func(_ string, args []string) ([]byte, error) {
		p := args[len(args)-1]
		switch {
		case strings.Contains(p, "/rulesets?"), strings.Contains(p, "/releases?"), strings.Contains(p, "/pulls?"),
			strings.Contains(p, "/files?"), strings.Contains(p, "/contents/"):
			return []byte("[]"), nil
		}
		return []byte("{}"), nil
	}}
	g := GHCLI{Run: rec.run}
	ctx := context.Background()
	r := Repo{"o", "r"}
	_ = g.AuthStatus(ctx)
	_, _ = g.Repo(ctx, r)
	_, _ = g.ActionsEnabled(ctx, r)
	_, _ = g.Workflow(ctx, r, "release.yml")
	_, _ = g.Runs(ctx, r, "release.yml", "push", "v1.0.0", testCommit)
	_, _ = g.RunByID(ctx, r, 1)
	_, _ = g.Jobs(ctx, r, 1)
	_, _ = g.Rulesets(ctx, r)
	_, _ = g.Environment(ctx, r, "release")
	_, _ = g.EnvironmentSecrets(ctx, r, "release")
	_, _ = g.Releases(ctx, r)
	_, _ = g.LatestRelease(ctx, r)
	_, _ = g.Contents(ctx, r, "", "main")
	_, _ = g.File(ctx, r, "x.rb", "main")
	_, _ = g.BranchHead(ctx, r, "main")
	_, _ = g.PullRequests(ctx, r, "o", "b")
	_, _ = g.PullRequestFiles(ctx, r, 1)
	_ = g.DownloadRelease(ctx, r, "v1.0.0", t.TempDir())
	_ = g.VerifyAttestation(ctx, "f", AttestPolicy{Repo: r, SignerWorkflow: "o/r/.github/workflows/release.yml", SourceRef: "refs/tags/v1.0.0", SourceDigest: testCommit})
	if len(rec.calls) < 19 {
		t.Fatalf("only %d calls", len(rec.calls))
	}
	for _, c := range rec.calls {
		cmd := strings.Join(c, " ")
		switch {
		case c[0] != "gh":
			t.Errorf("not gh: %s", cmd)
		case c[1] == "api":
			if c[2] != "--method" || c[3] != "GET" {
				t.Errorf("API call is not a GET: %s", cmd)
			}
		case strings.HasPrefix(cmd, "gh auth status"), strings.HasPrefix(cmd, "gh release download"):
		case strings.HasPrefix(cmd, "gh attestation verify"):
			for _, f := range []string{"--signer-workflow", "--source-ref", "--source-digest", "--deny-self-hosted-runners", "--repo"} {
				if !slices.Contains(c, f) {
					t.Errorf("attestation verify without %s: %s", f, cmd)
				}
			}
		default:
			t.Errorf("unexpected gh command: %s", cmd)
		}
	}
	if err := g.VerifyAttestation(ctx, "f", AttestPolicy{Repo: r}); err == nil {
		t.Error("an incomplete attestation policy was accepted")
	}
}

func TestGHCLIErrors(t *testing.T) {
	fail := func(stderr string, code int) error { return &CmdError{Cmd: "gh api", Code: code, Stderr: stderr} }
	n := 0
	rec := &recorder{reply: func(string, []string) ([]byte, error) {
		n++
		if n < 3 {
			return nil, fail("gh: Server Error (HTTP 502)", 1)
		}
		return []byte(`{"full_name":"o/r","default_branch":"main"}`), nil
	}}
	var slept []time.Duration
	g := GHCLI{Run: rec.run, Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	info, err := g.Repo(context.Background(), Repo{"o", "r"})
	if err != nil || info.DefaultBranch != "main" || len(slept) != 2 || slept[1] != 2*slept[0] {
		t.Fatalf("retry: %+v %v %v", info, err, slept)
	}

	rec.reply = func(string, []string) ([]byte, error) { return nil, fail("gh: Not Found (HTTP 404)", 1) }
	if _, err := g.LatestRelease(context.Background(), Repo{"o", "r"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	calls := len(rec.calls)
	rec.reply = func(string, []string) ([]byte, error) { return nil, fail("gh: Bad credentials (HTTP 401)", 1) }
	if _, err := g.Repo(context.Background(), Repo{"o", "r"}); err == nil || len(rec.calls) != calls+1 {
		t.Errorf("401 must not be retried: %v, %d calls", err, len(rec.calls)-calls)
	}
	for stderr, want := range map[string]bool{
		"gh: API rate limit exceeded (HTTP 403)": true, "gh: Resource not accessible (HTTP 403)": false,
		"gh: (HTTP 429)": true, "error connecting to api.github.com": true,
	} {
		if got := transient(fail(stderr, 1)); got != want {
			t.Errorf("transient(%q) = %t", stderr, got)
		}
	}
	if transient(&CmdError{Cmd: "gh", Code: -1, Stderr: "executable file not found"}) {
		t.Error("a missing gh is not transient")
	}
}

func TestGitCLI(t *testing.T) {
	rec := &recorder{reply: func(_ string, args []string) ([]byte, error) {
		switch args[0] {
		case "symbolic-ref":
			return nil, &CmdError{Cmd: "git symbolic-ref", Code: 1}
		case "rev-parse":
			if strings.HasPrefix(args[len(args)-1], "refs/tags/") {
				return nil, &CmdError{Cmd: "git rev-parse", Code: 1}
			}
			return []byte(testCommit + "\n"), nil
		case "ls-remote":
			return []byte(""), nil
		}
		return nil, nil
	}}
	g := GitCLI{Run: rec.run}
	ctx := context.Background()
	if b, err := g.CurrentBranch(ctx); err != nil || b != "" {
		t.Errorf("detached: %q %v", b, err)
	}
	if c, err := g.Commit(ctx, "refs/tags/v1.0.0"); err != nil || c != "" {
		t.Errorf("missing tag: %q %v", c, err)
	}
	if c, err := g.Commit(ctx, "HEAD"); err != nil || c != testCommit {
		t.Errorf("HEAD: %q %v", c, err)
	}
	if _, ok, err := g.RemoteTag(ctx, "v1.0.0"); ok || err != nil {
		t.Errorf("remote tag: %t %v", ok, err)
	}
	_ = g.FetchBranch(ctx, "main")
	_ = g.CreateTag(ctx, "v1.0.0", testCommit, "Release v1.0.0")
	_ = g.PushTag(ctx, "v1.0.0")
	if !slices.ContainsFunc(rec.calls, func(c []string) bool { return slices.Contains(c, "push") }) {
		t.Fatal("no push call recorded")
	}
	for _, c := range rec.calls {
		cmd := strings.Join(c, " ")
		for _, bad := range []string{"--force", "-f ", "+refs/tags", "--delete", "--tags", "--mirror", "--all"} {
			if strings.Contains(cmd+" ", bad) && !strings.HasPrefix(cmd, "git ls-remote --tags") {
				t.Errorf("unsafe git call: %s", cmd)
			}
		}
		sub := c[1]
		if sub == "-c" {
			if c[2] != "push.followTags=false" {
				t.Errorf("unexpected config override: %s", cmd)
			}
			sub = c[3]
		}
		switch sub {
		case "tag":
			if !slices.Contains(c, "--annotate") || c[len(c)-1] != testCommit {
				t.Errorf("tag must be annotated at the checked commit: %s", cmd)
			}
		case "push":
			if !slices.Contains(c, "--no-follow-tags") || !slices.Equal(c[len(c)-2:], []string{"origin", "refs/tags/v1.0.0:refs/tags/v1.0.0"}) {
				t.Errorf("push must push exactly the one tag: %s", cmd)
			}
		case "fetch":
			if !slices.Contains(c, "--no-tags") {
				t.Errorf("fetch must not fetch tags: %s", cmd)
			}
		}
	}
}
