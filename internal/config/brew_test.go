package config

import (
	"strings"
	"testing"
)

func TestBrewStrictSubstructures(t *testing.T) {
	cfg, err := Parse([]byte(`
project: {name: mycli}
brews:
  - name: mycli
    dependencies:
      - git
      - {name: go, type: build}
    repository:
      owner: acme
      name: homebrew-tap
      pull_request:
        enabled: true
        base: main
`))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Brews[0]
	if len(b.Dependencies) != 2 || b.Dependencies[0].Name != "git" || b.Dependencies[1].Type != "build" {
		t.Errorf("dependencies = %+v", b.Dependencies)
	}
	if base := b.Repository.PullRequest.Base; base == nil || base.Branch != "main" {
		t.Errorf("base = %+v", base)
	}

	for _, tc := range []struct{ yaml, err string }{
		{"brews:\n  - dependencies:\n      - {name: go, typ: build}\n", `unknown field (did you mean "type"?)`},
		{"brews:\n  - dependencies:\n      - {name: go, name: git}\n", "duplicate key"},
		{"brews:\n  - repository:\n      pull_request:\n        base: {owner: a, branche: x}\n", `unknown field (did you mean "branch"?)`},
		{"brews:\n  - repository:\n      pull_request:\n        base: [a]\n", "expected a mapping"},
	} {
		if _, err := Parse([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("Parse(%q) err = %v, want %q", tc.yaml, err, tc.err)
		}
	}
}

func TestValidateBrewNameStartsWithLetter(t *testing.T) {
	cfg := &Config{Project: Project{Name: "x"}, Brews: []Brew{{Name: "7zip"}}}
	cfg.ApplyDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must start with a letter") {
		t.Errorf("err = %v", err)
	}
}

func TestBrewAllowDowngrade(t *testing.T) {
	cfg, err := Parse([]byte("project: {name: mycli}\nbrews:\n  - name: a\n    allow_downgrade: true\n  - name: b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Brews[0].AllowDowngrade || cfg.Brews[1].AllowDowngrade {
		t.Fatalf("allow_downgrade = %t, %t", cfg.Brews[0].AllowDowngrade, cfg.Brews[1].AllowDowngrade)
	}
	if _, err := Parse([]byte("brews:\n  - allow_downgrade: maybe\n")); err == nil {
		t.Fatal("non-boolean allow_downgrade accepted")
	}
}

func TestBrewCompletions(t *testing.T) {
	cfg, err := Parse([]byte(`
project: {name: mycli}
brews:
  - name: mycli
    completions:
      executable: mycli
      generate:
        args: [completion]
        shell_parameter_format: flag
        shells: [bash, zsh]
      files:
        fish: completions/mycli.fish
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Brews[0].Completions
	if c == nil || c.Executable != "mycli" || c.Generate == nil || c.Generate.ShellParameterFormat != "flag" ||
		strings.Join(c.Generate.GeneratedShells(), ",") != "bash,zsh" || c.Files.Fish != "completions/mycli.fish" {
		t.Fatalf("completions %+v", c)
	}
	if got := (&BrewCompletionGenerate{}).GeneratedShells(); strings.Join(got, ",") != "bash,zsh,fish" {
		t.Fatalf("default shells %v", got)
	}
	if cfg, err := Parse([]byte("project: {name: mycli}\nbrews:\n  - name: mycli\n")); err != nil || cfg.Brews[0].Completions != nil {
		t.Fatalf("unset: %v %+v", err, cfg.Brews[0].Completions)
	}

	for _, tc := range []struct{ yaml, err string }{
		{"completions: {}", "set generate"},
		{"completions: {generate: {shells: [powershell]}}", `"powershell" must be bash, zsh or fish`},
		{"completions: {generate: {shells: [bash, bash]}}", `"bash" is listed twice`},
		{"completions: {generate: {shell_parameter_format: '--shell='}}", "must be one of arg, flag"},
		{"completions: {generate: {args: ['']}}", "non-empty argument"},
		{"completions: {generate: {args: [\"a\\nb\"]}}", "without control characters"},
		{"completions: {executable: bin/mycli, generate: {}}", "no path"},
		{"completions: {executable: '', files: {bash: ../x}}", "relative path inside the archive"},
		{"completions: {files: {bash: /etc/x}}", "relative path inside the archive"},
		{"completions: {generate: {shells: [bash]}, files: {bash: c.bash}}", "installed twice"},
		{"completions: {generate: {}, files: {zsh: _c}}", "also generated"},
		{"completions: {generate: {shell: [bash]}}", `unknown field (did you mean "shells"?)`},
		{"completions: {files: {powershell: x}}", "unknown field"},
	} {
		yml := "project: {name: mycli}\nbrews:\n  - name: mycli\n    " + tc.yaml + "\n"
		if _, err := load(yml); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err = %v, want %q", tc.yaml, err, tc.err)
		}
	}
}
