package tmpl

import (
	"errors"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	v := Vars{Project: "mycli", Version: "1.2.3", Commit: "abc123", ArtifactName: "x.zip", ArtifactExt: ".zip", Env: map[string]string{"FOO": "bar"}}.WithTarget("windows", "amd64", "")
	tests := []struct {
		in, want, err string
	}{
		{in: "plain", want: "plain"},
		{in: "{{ .Project }}_{{ .OS }}_{{ .Arch }}", want: "mycli_windows_amd64"},
		{in: "{{ .Os }}{{ .Ext }}", want: "windows.exe"},
		{in: "{{ .Env.FOO }}", want: "bar"},
		{in: "{{ .Env.MISSING }}", err: "MISSING"},
		{in: "{{ .Nope }}", err: "Nope"},
		{in: "{{ tolower \"ABC\" }}", want: "abc"},
		{in: "{{ replace .Version \".\" \"-\" }}", want: "1-2-3"},
		{in: "{{ title .OS }}", want: "Windows"},
		{in: "{{ .Project ", err: "invalid template"},
		{in: "{{ exec \"x\" }}", err: "exec"},
		{in: "{{ .FullCommit }}", want: "abc123"},
		{in: "{{ urlPathEscape \"a/b c\" }}", want: "a%2Fb%20c"},
		{in: "{{ .ArtifactName }}{{ .ArtifactExt }}", want: "x.zip.zip"},
	}
	for _, tt := range tests {
		got, err := Render(tt.in, v)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("Render(%q) error = %v, want containing %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Render(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestCheck(t *testing.T) {
	if err := Check("{{ .Env.ANYTHING }}_{{ .Version }}"); err != nil {
		t.Errorf("Check: %v", err)
	}
	if err := Check("{{ .ArtifactName }} {{ .FullCommit }} {{ urlPathEscape .Tag }}"); err != nil {
		t.Errorf("Check(GoReleaser compatibility names): %v", err)
	}
	if err := Check("{{ .Versoin }}"); err == nil {
		t.Error("Check accepted a misspelled field")
	}
}

func TestRenderOutputLimit(t *testing.T) {
	big := strings.Repeat("x", MaxOutput)
	if _, err := Render("{{ .Project }}{{ .Project }}", Vars{Project: big}); err == nil {
		t.Error("expected output limit error")
	}
}

func TestSecretVars(t *testing.T) {
	v := Vars{Env: map[string]string{"FOO": "config"}}
	if _, err := Render("{{ .Env.GITHUB_TOKEN }}", v); err == nil {
		t.Fatal("hidden variable rendered")
	}
	// Without a secret environment, RenderSecret sees .Env.
	if got, err := RenderSecret("{{ .Env.FOO }}", v); err != nil || got != "config" {
		t.Fatalf("RenderSecret without env: %q, %v", got, err)
	}
	v = WithSecretEnv(v, map[string]string{"GITHUB_TOKEN": "ghs_x", "FOO": "process"})
	if _, err := Render("{{ .Env.GITHUB_TOKEN }}", v); err == nil {
		t.Fatal("WithSecretEnv exposed the secret environment as .Env")
	}
	got, err := RenderSecret("{{ .Env.GITHUB_TOKEN }} {{ .Env.FOO }}", v)
	if err != nil || got != "ghs_x config" {
		t.Fatalf("RenderSecret = %q, %v (.Env overrides the process environment)", got, err)
	}
	// The secret environment survives copies such as WithTarget.
	if got, err := RenderSecret("{{ .Env.GITHUB_TOKEN }}", v.WithTarget("linux", "amd64", "")); err != nil || got != "ghs_x" {
		t.Fatalf("after WithTarget: %q, %v", got, err)
	}
}

// TestTemplatesCannotReachSecrets is a regression test for N3: the Secret
// method used to be callable from any template as .Secret.Env.NAME.
func TestTemplatesCannotReachSecrets(t *testing.T) {
	v := WithSecretEnv(Vars{}, map[string]string{"HOMEBREW_TAP_GITHUB_TOKEN": "github_pat_x"})
	for _, s := range []string{
		"{{ .Secret.Env.HOMEBREW_TAP_GITHUB_TOKEN }}",
		"{{ .secretEnv }}",
		"{{ (.WithSecretEnv .Env).Env }}",
		"{{ index .Env \"HOMEBREW_TAP_GITHUB_TOKEN\" }}",
	} {
		if got, err := Render(s, v); err == nil && strings.Contains(got, "github_pat_x") {
			t.Errorf("%s rendered %q", s, got)
		}
	}
	if err := Check("{{ .Secret.Env.X }}"); err == nil {
		t.Error("Check accepted .Secret")
	}
}

// TestRenderRefusesRegisteredSecrets covers the N3 defense in depth: a
// rendered (published) value never contains a registered secret.
func TestRenderRefusesRegisteredSecrets(t *testing.T) {
	const secret = "ghs_REGISTERED_SECRET_tmpl_test"
	RegisterSecret("", secret)
	v := WithSecretEnv(Vars{Env: map[string]string{"LEAK": "x" + secret + "y"}}, map[string]string{"T": secret})
	_, err := Render("desc {{ .Env.LEAK }}", v)
	if !errors.Is(err, ErrSecretInOutput) || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
	// The credential path is exempt.
	if got, err := RenderSecret("{{ .Env.T }}", v); err != nil || got != secret {
		t.Fatalf("RenderSecret = %q, %v", got, err)
	}
	// A plain value is returned as is (it is not a template).
	if got, err := Render("plain", v); err != nil || got != "plain" {
		t.Fatalf("plain = %q, %v", got, err)
	}
}

func TestEnvRefs(t *testing.T) {
	got, err := EnvRefs(`{{ .Env.TAP_PAT }}{{ if .Env.B }}{{ index .Env "C" }}{{ else }}{{ .Env.D | trim }}{{ end }}{{ with .Env }}{{ .X }}{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "B,C,D,TAP_PAT" {
		t.Errorf("EnvRefs = %v", got)
	}
	if got, err := EnvRefs("plain"); err != nil || got != nil {
		t.Errorf("plain: %v %v", got, err)
	}
	if _, err := EnvRefs("{{ .Env."); err == nil {
		t.Error("invalid template accepted")
	}
}

func TestIsCredentialName(t *testing.T) {
	for _, n := range []string{
		"GITHUB_TOKEN", "my_token", "AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "FTP_PASSWD", "SSH_PRIVATE_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS", "npmToken", "GPG_PASSPHRASE", "TAP_PAT", "SENTRY_API_KEY", "NPM_AUTH",
		"GPG_KEY", "MYAPIKEY", "AWS_ACCESS_KEY_ID", "SMTP_PASS", "DB_PWD", "pat", "KEY", "MYSQL_PWD", "PWD_HASH",
	} {
		if !IsCredentialName(n) {
			t.Errorf("%s: not a credential", n)
		}
	}
	for _, n := range []string{"HOME", "PATH", "GOPATH", "GOOS", "API_URL", "VERSION", "KEYCHAIN", "KEYBOARD", "AUTHOR", "PATCH", "COMPASS", "PASSENGER_ENV", "PWD", "OLDPWD", "pwd", "TMPDIR", "GITHUB_WORKSPACE", "RUNNER_TEMP"} {
		if IsCredentialName(n) {
			t.Errorf("%s: credential", n)
		}
	}
}
