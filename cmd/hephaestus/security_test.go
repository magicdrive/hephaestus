package hephaestus

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/tmpl"
	"github.com/magicdrive/hephaestus/internal/version"
)

const (
	secretRelease = "ghs_RELEASESECRET0123456789"
	secretTap     = "github_pat_TAPSECRET0123456789"
)

// commandProject writes a project with one shell command build.
func commandProject(t *testing.T, yml string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell command builds use sh")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hephaestus.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readOutputs(t *testing.T) (string, string) {
	t.Helper()
	out, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	sum, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	return string(out), string(sum)
}

// TestBuildDoesNotInheritTokens is a regression test for F1: build commands
// used to see the release and tap tokens.
func TestBuildDoesNotInheritTokens(t *testing.T) {
	clearGitHubEnv(t)
	root := commandProject(t, `
project: {name: tool}
builds:
  - id: b
    targets: [{os: linux, arch: amd64}]
    command: 'env > leaked.txt; echo x > "$HEPHAESTUS_OUTPUT"'
    output: dist/bin/tool
`)
	t.Setenv("GITHUB_TOKEN", secretRelease)
	t.Setenv("HOMEBREW_TAP_GITHUB_TOKEN", secretTap)
	t.Setenv("MY_CUSTOM_TOKEN_VAR", "custom-release-token")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "runtime-token-value")
	t.Setenv("HEPH_KEEP", "kept")
	code, _, errs := runCLI(t, "build", "--config", filepath.Join(root, "hephaestus.yaml"), "--snapshot", "--token-env", "MY_CUSTOM_TOKEN_VAR")
	if code != 0 {
		t.Fatalf("build: %s", errs)
	}
	leaked, err := os.ReadFile(filepath.Join(root, "leaked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{secretRelease, secretTap, "custom-release-token", "runtime-token-value"} {
		if strings.Contains(string(leaked), s) {
			t.Errorf("build command saw %q", s)
		}
	}
	if !strings.Contains(string(leaked), "HEPH_KEEP=kept") {
		t.Error("ordinary variables must still be inherited")
	}
}

// TestSecretsRedacted is a regression test for F14: a secret passed to a
// build (explicit opt-in) and echoed in its failure used to reach stderr,
// the step summary and the outputs.
func TestSecretsRedacted(t *testing.T) {
	clearGitHubEnv(t)
	root := commandProject(t, `
project: {name: tool}
builds:
  - id: b
    targets: [{os: linux, arch: amd64}]
    command: 'echo "token is $T" >&2; exit 1'
    output: dist/bin/tool
    env:
      T: "{{ .Env.GITHUB_TOKEN }}"
`)
	t.Setenv("GITHUB_TOKEN", secretRelease)
	code, _, errs := runCLI(t, "build", "--config", filepath.Join(root, "hephaestus.yaml"), "--snapshot")
	if code != 1 {
		t.Fatalf("build should fail: %s", errs)
	}
	out, sum := readOutputs(t)
	for name, s := range map[string]string{"stderr": errs, "outputs": out, "summary": sum} {
		if strings.Contains(s, secretRelease) {
			t.Errorf("%s leaks the token:\n%s", name, s)
		}
	}
	if !strings.Contains(errs, "token is ***") || !strings.Contains(sum, "token is ***") {
		t.Errorf("redacted failure missing:\nstderr:\n%s\nsummary:\n%s", errs, sum)
	}
}

// TestTemplateEnvHidesCredentials is a regression test for F9: .Env used to
// expose every variable, tokens included, to published values.
func TestTemplateEnvHidesCredentials(t *testing.T) {
	cfg := &config.Config{Env: []config.EnvEntry{"DEPLOY_TOKEN=declared", "PLAIN=config"}}
	environ := []string{
		"GITHUB_TOKEN=" + secretRelease, "HOMEBREW_TAP_GITHUB_TOKEN=" + secretTap,
		"RELEASE_KEY_VAR=custom", "ACTIONS_ID_TOKEN_REQUEST_URL=https://oidc",
		"AWS_SECRET_ACCESS_KEY=aws", "DB_password=pw", "SSH_PRIVATE_KEY=k", "GCP_CREDENTIALS=c", "FTP_PASSWD=p",
		"DEPLOY_TOKEN=process", "PLAIN=process", "PATH=/bin", "GOOS=linux",
	}
	v := templateVars(cfg, version.Info{Version: "1.0.0"}, "", environ, "GITHUB_TOKEN", "RELEASE_KEY_VAR")
	for _, k := range []string{"GITHUB_TOKEN", "HOMEBREW_TAP_GITHUB_TOKEN", "RELEASE_KEY_VAR", "ACTIONS_ID_TOKEN_REQUEST_URL", "AWS_SECRET_ACCESS_KEY", "DB_password", "SSH_PRIVATE_KEY", "GCP_CREDENTIALS", "FTP_PASSWD"} {
		if _, ok := v.Env[k]; ok {
			t.Errorf(".Env exposes %s", k)
		}
		if _, err := tmpl.Render("{{ .Env."+k+" }}", v); err == nil {
			t.Errorf("{{ .Env.%s }} rendered", k)
		}
	}
	for k, want := range map[string]string{"DEPLOY_TOKEN": "declared", "PLAIN": "config", "PATH": "/bin", "GOOS": "linux"} {
		if v.Env[k] != want {
			t.Errorf(".Env.%s = %q, want %q", k, v.Env[k], want)
		}
	}
	// The credential-reading path (repository.token, build env) sees all.
	s := v.Secret()
	if s.Env["GITHUB_TOKEN"] != secretRelease || s.Env["RELEASE_KEY_VAR"] != "custom" || s.Env["DEPLOY_TOKEN"] != "declared" {
		t.Errorf("Secret env = %v", s.Env)
	}
}

// TestCheckAcceptsCredentialTemplates guards against false positives:
// configuration validation must not reject templates that read a hidden
// variable (repository.token in particular).
func TestCheckAcceptsCredentialTemplates(t *testing.T) {
	root := project(t, `brews:
  - repository: {owner: o, name: homebrew-tap, token: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"}
`)
	code, out, errs := runCLI(t, "check", "--config", filepath.Join(root, "hephaestus.yaml"))
	if code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("check: %d\n%s\n%s", code, out, errs)
	}
}

// TestLiteralTokenWarning covers F19: a token written in the configuration
// is reported (without echoing it) and redacted from all output.
func TestLiteralTokenWarning(t *testing.T) {
	const lit = "ghp_LITERALTOKEN0123456789abcdef"
	root := project(t, `env: ["API=`+lit+`"]
brews:
  - repository: {owner: o, name: homebrew-tap, token: "plain-token-value"}
    description: "`+lit+`"
`)
	code, out, errs := runCLI(t, "check", "--config", filepath.Join(root, "hephaestus.yaml"))
	if code != 0 {
		t.Fatalf("check: %s", errs)
	}
	for _, want := range []string{"env[0] looks like a GitHub token", "brews[0].description looks like a GitHub token", "brews[0].repository.token is a literal value"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing warning %q:\n%s", want, errs)
		}
	}
	if strings.Contains(errs+out, lit) || strings.Contains(errs+out, "plain-token-value") {
		t.Errorf("literal token echoed:\n%s%s", out, errs)
	}
}

// TestPanicStillWritesOutputs is a regression test for F11: a panic in a
// stage used to crash the process without outputs or summary.
func TestPanicStillWritesOutputs(t *testing.T) {
	clearGitHubEnv(t)
	orig := executeFn
	t.Cleanup(func() { executeFn = orig })
	executeFn = func(context.Context, *config.Config, *Options, *report) error {
		panic("value is not valid UTF-8")
	}
	var errb bytes.Buffer
	code := run(context.Background(), &config.Config{}, &Options{Stdout: &errb, Stderr: &errb})
	if code != 1 || !strings.Contains(errb.String(), "internal error") {
		t.Fatalf("code %d:\n%s", code, errb.String())
	}
	out, sum := readOutputs(t)
	if !strings.Contains(out, "failed") || !strings.Contains(sum, "internal error") {
		t.Errorf("outputs:\n%s\nsummary:\n%s", out, sum)
	}
}

// TestBadAPIURLReported is a regression test for F20 and F3: an invalid or
// plain-http API URL used to surface as "token is required", or send the
// token in clear text.
func TestBadAPIURLReported(t *testing.T) {
	for name, tc := range map[string]struct{ extra, env string }{
		"config http":  {extra: "release:\n  github: {owner: o, name: r, api_url: \"http://evil.example\"}\n"},
		"env http":     {extra: "release:\n  github: {owner: o, name: r}\n", env: "http://evil.example/api/v3"},
		"upload http":  {extra: "release:\n  github: {owner: o, name: r, upload_url: \"http://evil.example\"}\n"},
		"config query": {extra: "release:\n  github: {owner: o, name: r, api_url: \"https://x.example/?a=b\"}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			clearGitHubEnv(t)
			t.Setenv("GITHUB_API_URL", tc.env)
			t.Setenv("GITHUB_TOKEN", secretRelease)
			root := project(t, tc.extra)
			code, _, errs := runCLI(t, "run", "--config", filepath.Join(root, "hephaestus.yaml"), "--tag", "v1.0.0")
			if code != 1 || !strings.Contains(errs, "github: invalid") || strings.Contains(errs, "token is required") {
				t.Fatalf("code %d:\n%s", code, errs)
			}
			// The URL is checked before any stage runs.
			if _, err := os.Stat(filepath.Join(root, "dist")); !os.IsNotExist(err) {
				t.Error("a stage ran before the API URL was validated")
			}
		})
	}
}

func TestRedactor(t *testing.T) {
	r := &redactor{}
	r.add("", "abc", "  sekret-value  ", "sekret-value-longer")
	if got := r.redact("x sekret-value-longer y sekret-value abc"); got != "x *** y *** abc" {
		t.Errorf("redact = %q", got)
	}
	r.addEnviron([]string{"MY_TOK=custom-tok", "NPM_TOKEN=short", "OTHER_SECRET=long-enough-secret", "HOME=/home/u", "GH_TOKEN=ghtoken1"}, "MY_TOK")
	if got := r.redact("custom-tok short long-enough-secret /home/u ghtoken1"); got != "*** short *** /home/u ***" {
		t.Errorf("redact env = %q", got)
	}

	// The writer catches a secret split across writes, and flushes.
	var buf bytes.Buffer
	w := r.writer(&buf)
	for _, p := range []string{"one sekr", "et-value two\nthree sekret", "-value"} {
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if buf.String() != "one *** two\n" {
		t.Errorf("before flush: %q", buf.String())
	}
	w.Flush()
	if buf.String() != "one *** two\nthree ***" {
		t.Errorf("after flush: %q", buf.String())
	}

	// A very long line is emitted without waiting for a newline, still
	// redacted at the cut.
	buf.Reset()
	long := strings.Repeat("a", redactLineMax-5) + "sekret-value-longer" + strings.Repeat("b", 10)
	_, _ = w.Write([]byte(long))
	w.Flush()
	if strings.Contains(buf.String(), "sekret") || !strings.Contains(buf.String(), "a***b") || buf.Len() != len(long)-len("sekret-value-longer")+3 {
		t.Errorf("long line not redacted (len %d)", buf.Len())
	}
}
