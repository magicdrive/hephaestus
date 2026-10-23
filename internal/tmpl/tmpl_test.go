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
