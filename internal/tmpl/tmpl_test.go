package tmpl

import (
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
		t.Fatal("hidden variable rendered without Secret")
	}
	// Without a secret environment, Secret is v itself.
	if got, err := Render("{{ .Env.FOO }}", v.Secret()); err != nil || got != "config" {
		t.Fatalf("Secret without env: %q, %v", got, err)
	}
	v = v.WithSecretEnv(map[string]string{"GITHUB_TOKEN": "ghs_x", "FOO": "process"})
	if _, err := Render("{{ .Env.GITHUB_TOKEN }}", v); err == nil {
		t.Fatal("WithSecretEnv exposed the secret environment as .Env")
	}
	got, err := Render("{{ .Env.GITHUB_TOKEN }} {{ .Env.FOO }}", v.Secret())
	if err != nil || got != "ghs_x config" {
		t.Fatalf("Secret render = %q, %v (.Env overrides the process environment)", got, err)
	}
	// The secret environment survives copies such as WithTarget.
	if got, err := Render("{{ .Env.GITHUB_TOKEN }}", v.WithTarget("linux", "amd64", "").Secret()); err != nil || got != "ghs_x" {
		t.Fatalf("after WithTarget: %q, %v", got, err)
	}
	// Templates cannot reach the unexported field.
	if _, err := Render("{{ .secretEnv }}", v); err == nil {
		t.Fatal("unexported field reachable")
	}
}

func TestIsCredentialName(t *testing.T) {
	for _, n := range []string{"GITHUB_TOKEN", "my_token", "AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "FTP_PASSWD", "SSH_PRIVATE_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "npmToken"} {
		if !IsCredentialName(n) {
			t.Errorf("%s: not a credential", n)
		}
	}
	for _, n := range []string{"HOME", "PATH", "GOOS", "API_URL", "VERSION", "KEYCHAIN"} {
		if IsCredentialName(n) {
			t.Errorf("%s: credential", n)
		}
	}
}
