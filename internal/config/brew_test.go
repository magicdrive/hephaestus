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
