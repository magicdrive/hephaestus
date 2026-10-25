package hephaestus

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/pipeline"
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
	got, err := tmpl.RenderSecret("{{ .Env.GITHUB_TOKEN }} {{ .Env.RELEASE_KEY_VAR }} {{ .Env.DEPLOY_TOKEN }}", v)
	if err != nil || got != secretRelease+" custom declared" {
		t.Errorf("RenderSecret = %q, %v", got, err)
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
	r.add("", "  sekret-value  ", "sekret-value-longer")
	if got := r.redact("x sekret-value-longer y sekret-value abc"); got != "x *** y *** abc" {
		t.Errorf("redact = %q", got)
	}
	// A known token is redacted whatever its length; a variable that only
	// looks like a credential by name needs 8 characters.
	r.addEnviron([]string{"MY_TOK=ct1", "NPM_TOKEN=short", "OTHER_SECRET=long-enough-secret", "HOME=/home/u", "GH_TOKEN=gh1"}, "MY_TOK")
	if got := r.redact("<ct1> short long-enough-secret /home/u <gh1>"); got != "<***> short *** /home/u <***>" {
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

// TestRedactorEncodedForms is a regression test for N5 (secreview p13):
// base64 (also as x-access-token:<token> basic credentials), URL-encoded and
// %q-escaped forms of a secret used to pass through.
func TestRedactorEncodedForms(t *testing.T) {
	const tok = "ghs_SECRET/VALUE+123\"x"
	r := &redactor{}
	r.add(tok)
	forms := map[string]string{
		"b64":         base64.StdEncoding.EncodeToString([]byte(tok)),
		"b64url":      base64.URLEncoding.EncodeToString([]byte(tok)),
		"b64raw":      base64.RawStdEncoding.EncodeToString([]byte(tok)),
		"basic":       base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok)),
		"basicurl":    base64.RawURLEncoding.EncodeToString([]byte("x-access-token:" + tok)),
		"query":       url.QueryEscape(tok),
		"percent-all": "ghs%5FSECRET%2FVALUE%2B123%22x",
		"path":        url.PathEscape(tok),
		"quoted":      fmt.Sprintf("%q", tok),
		"json":        string(must(json.Marshal(tok))),
		"header line": "Authorization: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok)),
	}
	for name, f := range forms {
		got := r.redact("[" + f + "]")
		if strings.Contains(got, strings.Trim(f, `"`)) || !strings.Contains(got, "***") {
			t.Errorf("%s: %q -> %q", name, f, got)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestEncodedSecretsRedactedFromSummary checks, end to end, that the stderr
// tail quoted in a build failure (stderr, step summary, outputs) has the
// token and its encoded forms redacted, for a short token too.
func TestEncodedSecretsRedactedFromSummary(t *testing.T) {
	clearGitHubEnv(t)
	const tok = "tok_42"
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	root := commandProject(t, `
project: {name: tool}
builds:
  - id: b
    targets: [{os: linux, arch: amd64}]
    command: 'printf "raw=%s\nb64=%s\nurl=%s\n" "$T" "$(printf x-access-token:%s "$T" | base64)" "tok%5F42" >&2; exit 1'
    output: dist/bin/tool
    env:
      T: "{{ .Env.GITHUB_TOKEN }}"
`)
	t.Setenv("GITHUB_TOKEN", tok)
	code, _, errs := runCLI(t, "build", "--config", filepath.Join(root, "hephaestus.yaml"), "--snapshot")
	if code != 1 {
		t.Fatalf("build should fail: %s", errs)
	}
	out, sum := readOutputs(t)
	for name, s := range map[string]string{"stderr": errs, "outputs": out, "summary": sum} {
		for _, f := range []string{tok, basic, "tok%5F42"} {
			if strings.Contains(s, f) {
				t.Errorf("%s contains %q:\n%s", name, f, s)
			}
		}
	}
	if !strings.Contains(sum, "raw=***") || !strings.Contains(sum, "b64=***") || !strings.Contains(sum, "url=***") {
		t.Errorf("summary:\n%s", sum)
	}
}

// TestBuildDoesNotInheritCredentialVariables is a regression test for N2
// (secreview p10): a variable read by repository.token, or named like a
// credential, used to reach build commands.
func TestBuildDoesNotInheritCredentialVariables(t *testing.T) {
	clearGitHubEnv(t)
	root := commandProject(t, `
project: {name: tool}
builds:
  - id: b
    targets: [{os: linux, arch: amd64}]
    command: 'env > leaked.txt; echo x > "$HEPHAESTUS_OUTPUT"'
    output: dist/bin/tool
    env:
      OPTED_IN: "{{ .Env.NPM_AUTH }}"
brews:
  - repository: {owner: o, name: homebrew-tap, token: "{{ .Env.TAP_WRITE }}"}
    url_template: "https://example.com/{{ .ArtifactName }}"
`)
	t.Setenv("TAP_WRITE", "tap_write_SECRET3")
	t.Setenv("TAP_PAT", "tap_pat_SECRET4")
	t.Setenv("SENTRY_API_KEY", "sentry_SECRET5")
	t.Setenv("NPM_AUTH", "npm_SECRET6")
	code, _, errs := runCLI(t, "build", "--config", filepath.Join(root, "hephaestus.yaml"), "--snapshot")
	if code != 0 {
		t.Fatalf("build: %s", errs)
	}
	leaked := string(must(os.ReadFile(filepath.Join(root, "leaked.txt"))))
	for _, s := range []string{"TAP_WRITE=", "TAP_PAT=", "SENTRY_API_KEY=", "NPM_AUTH="} {
		if strings.Contains(leaked, s) {
			t.Errorf("build command saw %s", s)
		}
	}
	if !strings.Contains(leaked, "OPTED_IN=npm_SECRET6") {
		t.Error("a credential named in the build's env must be passed (opt-in)")
	}
}

// formulaProject is project() with a formula that renders desc.
func formulaProject(t *testing.T, desc string) string {
	return project(t, `brews:
  - repository: {owner: o, name: homebrew-tap}
    url_template: "https://example.com/{{ .ArtifactName }}"
    description: "`+desc+`"
`)
}

// TestPublishedValuesCannotReadSecrets is a regression test for N3 and N6
// (secreview p11): .Secret.Env.NAME was callable from any template, and
// TAP_PAT-style names were not hidden from .Env.
func TestPublishedValuesCannotReadSecrets(t *testing.T) {
	clearGitHubEnv(t)
	t.Setenv("HOMEBREW_TAP_GITHUB_TOKEN", secretTap)
	t.Setenv("TAP_PAT", "tap_pat_SECRET4")
	t.Setenv("SENTRY_API_KEY", "sentry_SECRET5")
	for _, desc := range []string{
		"via-method={{ .Secret.Env.HOMEBREW_TAP_GITHUB_TOKEN }}",
		"{{ .Env.TAP_PAT }}",
		"{{ .Env.SENTRY_API_KEY }}",
	} {
		root := formulaProject(t, desc)
		code, _, errs := runCLI(t, "run", "--config", filepath.Join(root, "hephaestus.yaml"), "--tag", "v1.0.0", "--stages", "build,archive,checksum,formula")
		if code == 0 || !(strings.Contains(errs, "can't evaluate field Secret") || strings.Contains(errs, "no entry for key")) {
			t.Errorf("%s: code %d, want a template error:\n%s", desc, code, errs)
		}
		formula, _ := os.ReadFile(filepath.Join(root, "dist", "homebrew", "tool.rb"))
		for _, s := range []string{secretTap, "tap_pat_SECRET4", "sentry_SECRET5"} {
			if strings.Contains(string(formula)+errs, s) {
				t.Errorf("%s: leaked %s", desc, s)
			}
		}
	}
}

// TestPublishedValuesRefuseTokenValues covers the N3 defense in depth: a
// token reaching a published value under another name is refused.
func TestPublishedValuesRefuseTokenValues(t *testing.T) {
	clearGitHubEnv(t)
	t.Setenv("HOMEBREW_TAP_GITHUB_TOKEN", secretTap)
	t.Setenv("INNOCENT_LOOKING", "x"+secretTap)
	root := formulaProject(t, "{{ .Env.INNOCENT_LOOKING }}")
	code, _, errs := runCLI(t, "run", "--config", filepath.Join(root, "hephaestus.yaml"), "--tag", "v1.0.0", "--stages", "build,archive,checksum,formula")
	if code != 1 || !strings.Contains(errs, "template output contains a credential") || strings.Contains(errs, secretTap) {
		t.Fatalf("code %d:\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "dist", "homebrew", "tool.rb")); err == nil {
		t.Error("formula written")
	}
}

func TestPhase(t *testing.T) {
	clearGitHubEnv(t)
	root := project(t, `brews:
  - repository: {owner: o, name: homebrew-tap}
    url_template: "https://example.com/{{ .ArtifactName }}"
    skip_upload: "true"
`)
	cfg := filepath.Join(root, "hephaestus.yaml")
	code, _, errs := runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0", "--phase", "local")
	if code != 0 || !strings.Contains(errs, "==> checksum") || strings.Contains(errs, "==> formula") {
		t.Fatalf("local: %d\n%s", code, errs)
	}
	code, _, errs = runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0", "--phase", "publish")
	if code != 0 || !strings.Contains(errs, "==> formula") || strings.Contains(errs, "==> build") {
		t.Fatalf("publish: %d\n%s", code, errs)
	}
	// Explicit stages are intersected with the phase; an empty phase does
	// nothing and writes no outputs (the action then uses the other step's).
	os.Remove(os.Getenv("GITHUB_OUTPUT"))
	code, _, errs = runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0", "--stages", "build,archive", "--phase", "publish")
	if code != 0 || !strings.Contains(errs, "no stages to run") {
		t.Fatalf("empty phase: %d\n%s", code, errs)
	}
	if _, err := os.Stat(os.Getenv("GITHUB_OUTPUT")); err == nil {
		t.Error("an empty phase wrote outputs")
	}
	if code, _, _ := runCLI(t, "run", "--config", cfg, "--phase", "deploy"); code != 2 {
		t.Errorf("invalid phase: %d", code)
	}
	if got := inPhase([]pipeline.Stage{pipeline.Tap, pipeline.Build, pipeline.Formula, pipeline.Checksum}, phaseLocal); len(got) != 2 || got[0] != pipeline.Build {
		t.Errorf("inPhase = %v", got)
	}
}

// TestWarnsWhenBuildAndPublishShareCredentials covers N1: one process that
// builds and publishes with a token in its environment is warned about.
func TestWarnsWhenBuildAndPublishShareCredentials(t *testing.T) {
	clearGitHubEnv(t)
	t.Setenv("GITHUB_TOKEN", secretRelease)
	t.Setenv("GITHUB_API_URL", "http://127.0.0.1:1") // never reached successfully
	root := project(t, "release:\n  github: {owner: o, name: r}\n")
	cfg := filepath.Join(root, "hephaestus.yaml")
	_, _, errs := runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0")
	if !strings.Contains(errs, "run build and publish in separate jobs") {
		t.Errorf("no warning:\n%s", errs)
	}
	_, _, errs = runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0", "--phase", "publish")
	if strings.Contains(errs, "separate jobs") {
		t.Errorf("publish phase warned:\n%s", errs)
	}
}

// The environment of a runner or a shell: directories under names that look
// like credentials must not hide paths, and tokens must still be hidden,
// including when they overlap with paths.
func TestRedactor_EnvironmentPaths(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "proj", "ウェブ app") // Unicode and a space
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "creds.json")
	if err := os.WriteFile(keyFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	winPath := `C:\Users\runner\work\proj`
	r := &redactor{}
	r.addEnviron([]string{
		"PWD=" + nested,
		"OLDPWD=" + root,
		"HOME=" + root,
		"TMPDIR=" + root,
		"PATH=" + root + string(os.PathListSeparator) + "/usr/bin",
		"GITHUB_WORKSPACE=" + nested,
		"GOOGLE_APPLICATION_CREDENTIALS=" + keyFile, // where a credential is
		"SSH_AUTH_SOCK=" + root,
		"KEY_DIR=" + winPath, // not an existing absolute path here: redacted
		"GITHUB_TOKEN=ghs_runnerTokenValue1234",
		"TAP_PAT=github_pat_tapValue5678",
		"DB_PASSWORD=hunter2-hunter2",
		"EMPTY_TOKEN=",
		"SPACES_SECRET=   ",
		// A secret that is also part of a path, and a path-like secret
		// under a known token name: both stay hidden.
		"NPM_TOKEN=proj-secret-segment",
		"GH_TOKEN=" + nested,
	})

	in := strings.Join([]string{
		"root " + nested + "/dist/x.tar.gz",
		"home " + root,
		"creds " + keyFile,
		"win " + winPath,
		"tokens ghs_runnerTokenValue1234 github_pat_tapValue5678 hunter2-hunter2",
		"overlap /work/proj-secret-segment/file",
	}, "\n")
	got := r.redact(in)

	for _, leaked := range []string{"ghs_runnerTokenValue1234", "github_pat_tapValue5678", "hunter2-hunter2", "proj-secret-segment", winPath} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q leaked:\n%s", leaked, got)
		}
	}
	// GH_TOKEN is a known token variable: its value is hidden even though
	// it is an existing directory. Every other path stays readable.
	if strings.Contains(got, nested) {
		t.Errorf("the value of GH_TOKEN (a directory) was not redacted:\n%s", got)
	}
	for _, want := range []string{"home " + root, "creds " + keyFile} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was redacted:\n%s", want, got)
		}
	}
	if strings.Contains(got, "***\n***") || strings.Count(got, redacted) > 7 {
		t.Errorf("over-redacted:\n%s", got)
	}

	// Without GH_TOKEN naming it, the working directory stays readable.
	r2 := &redactor{}
	r2.addEnviron([]string{"PWD=" + nested, "GITHUB_WORKSPACE=" + nested, "OLDPWD=" + root, "EMPTY_TOKEN="})
	line := "root " + nested + "/dist"
	if got := r2.redact(line); got != line {
		t.Errorf("PWD redacted a path: %q", got)
	}
}

// hephaestus check prints the project root; a shell's PWD (the project
// directory) must not turn it into "***".
func TestCheck_PWDIsNotRedacted(t *testing.T) {
	clearGitHubEnv(t)
	root := filepath.Join(t.TempDir(), "my project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "hephaestus.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\nproject:\n  name: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", root)
	t.Setenv("OLDPWD", root)
	t.Setenv("HEPH_TEST_TOKEN", "heph-test-token-value-0001")
	code, out, errs := runCLI(t, "check", "--config", cfg)
	if code != 0 {
		t.Fatalf("check: %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "root "+root) || strings.Contains(out, redacted) {
		t.Errorf("check output:\n%s", out)
	}
}
