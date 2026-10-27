package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func load(yml string) (*Config, error) { return Load(strings.NewReader(yml), "/project") }

func TestMinimal(t *testing.T) {
	cfg, err := load("project: {name: mycli}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 || cfg.Dist != "dist" || cfg.Manifest != "dist/hephaestus-manifest.json" || cfg.Root != "/project" {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.Limits.MaxFiles != DefaultMaxFiles || cfg.Checksum.Name != DefaultChecksumName {
		t.Error("limit/checksum defaults")
	}
}

func TestFullExample(t *testing.T) {
	cfg, err := load(`
version: 1
project: {name: mycli, description: Forge things, homepage: "https://example.com", license: MIT}
env: [FOO=bar]
builds:
  - id: default
    matrix: {os: [darwin, linux], arch: [amd64, arm64], exclude: [{os: linux, arch: arm64}]}
    command: make build
    env: {TARGET_OS: "{{ .OS }}"}
    timeout: 5m
    output: dist/{{ .OS }}/{{ .Arch }}/mycli
archives:
  - name: "{{ .Project }}_{{ .OS }}_{{ .Arch }}"
    format: tar.gz
    format_overrides: [{os: windows, format: zip}]
    files:
      - {src: "dist/{{ .OS }}/{{ .Arch }}/mycli", dst: mycli, mode: "0755"}
      - README.md
      - {src: "docs/**/*.md", dst: docs/, exclude: ["docs/internal/**"]}
      - {src: docs/man/mycli.1, dst: man/mycli.1, required: false, when: {os: [darwin, linux]}}
release:
  github: {enabled: true, draft: false}
homebrew:
  repository: {owner: magicdrive, name: homebrew-tap}
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ExpandTargets(cfg.Builds[0].Targets, cfg.Builds[0].Matrix)); got != 3 {
		t.Errorf("targets = %d", got)
	}
	if cfg.Builds[0].Command.Shell != "make build" || cfg.Builds[0].Provider() != ProviderCommand {
		t.Error("command")
	}
	if !cfg.Archives[0].FilesDeclared() || cfg.Archives[0].Files[1].Src != "README.md" || !cfg.Archives[0].Files[0].IsRequired() || cfg.Archives[0].Files[3].IsRequired() {
		t.Error("files")
	}
	if len(cfg.Brews) != 1 || cfg.Brews[0].Name != "mycli" || cfg.Brews[0].License != "MIT" || cfg.Homebrew != nil {
		t.Errorf("brews = %+v", cfg.Brews)
	}
	if !cfg.Release.GitHub.IsEnabled() || cfg.Release.GitHub.Prerelease != "auto" {
		t.Error("release defaults")
	}
}

func TestUnknownFieldsReportLine(t *testing.T) {
	_, err := load("project: {name: x}\narchives:\n  - files:\n      - src: a\n        dest: b\n")
	if err == nil {
		t.Fatal("unknown field accepted")
	}
	msg := err.Error()
	for _, want := range []string{"line 5", "archives[0].files[0].dest", "unknown field", `did you mean "dst"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if _, err := load("project: {name: x, nmae: y}\n"); err == nil {
		t.Error("unknown nested field accepted")
	}
	if _, err := load("project: {name: x}\nproject: {name: y}\n"); err == nil {
		t.Error("duplicate key accepted")
	}
}

func TestValidationErrorsAreLocated(t *testing.T) {
	_, err := load(`project: {name: x}
archives:
  - targets: [{os: linux, arch: amd64}]
    format: rar
    files: [{src: a, mode: "0777"}]
`)
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"line 4: archives[0].format", "line 5: archives[0].files[0].mode", "world-writable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
}

func TestPreflightSection(t *testing.T) {
	cfg, err := load("project: {name: x}\npreflight: {remote: upstream, branch: release/1.x, workflow: release.yaml, environment: release, secrets: [SIGNING_KEY], tap_token_secret: TAP, skip: [github.ruleset.branch]}\n")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Preflight
	if p.Remote != "upstream" || p.Branch != "release/1.x" || p.Workflow != "release.yaml" || p.Environment != "release" ||
		len(p.Secrets) != 1 || p.TapTokenSecret != "TAP" || len(p.Skip) != 1 {
		t.Errorf("preflight %+v", p)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"empty":                   "",
		"not mapping":             "- a\n",
		"two documents":           "project: {name: x}\n---\nproject: {name: y}\n",
		"alias":                   "a: &x {name: y}\nproject: *x\n",
		"no name":                 "project: {}\n",
		"bad name":                "project: {name: '../x'}\n",
		"future version":          "version: 2\nproject: {name: x}\n",
		"two providers":           "project: {name: x}\nbuilds: [{command: make, existing: {path: a}, output: a, targets: [{os: linux, arch: amd64}]}]\n",
		"no targets":              "project: {name: x}\nbuilds: [{command: make, output: a}]\n",
		"bad os":                  "project: {name: x}\nbuilds: [{command: make, output: a, targets: [{os: Linux, arch: amd64}]}]\n",
		"dup target":              "project: {name: x}\nbuilds: [{command: make, output: a, targets: [{os: linux, arch: amd64}], matrix: {os: [linux], arch: [amd64]}}]\n",
		"bad template":            "project: {name: x}\narchives: [{name: '{{ .Nope }}'}]\n",
		"unknown build":           "project: {name: x}\narchives: [{builds: [nope]}]\n",
		"dup archive ids":         "project: {name: x}\narchives: [{id: a}, {id: a}]\n",
		"missing ids":             "project: {name: x}\narchives: [{}, {}]\n",
		"bad dist":                "project: {name: x}\ndist: /tmp\n",
		"bad symlinks":            "project: {name: x}\narchives: [{symlinks: maybe}]\n",
		"bad size":                "project: {name: x}\nlimits: {max_file_size: 1GB}\n",
		"bad duration":            "project: {name: x}\nbuilds: [{command: make, output: a, timeout: soon, targets: [{os: linux, arch: amd64}]}]\n",
		"plist":                   "project: {name: x}\nbrews: [{plist: x}]\n",
		"bad formula name":        "project: {name: x}\nbrews: [{name: My_Tool}]\n",
		"preflight workflow path": "project: {name: x}\npreflight: {workflow: ../release.yml}\n",
		"preflight workflow name": "project: {name: x}\npreflight: {workflow: release}\n",
		"preflight bad secret":    "project: {name: x}\npreflight: {secrets: ['A B']}\n",
		"preflight bad check ID":  "project: {name: x}\npreflight: {skip: [Git.Branch]}\n",
		"preflight unknown key":   "project: {name: x}\npreflight: {enviroment: release}\n",
		"preflight bad remote":    "project: {name: x}\npreflight: {remote: '-x'}\n",
		"bad dependency":          "project: {name: x}\nbrews: [{dependencies: ['a; system']}]\n",
		"bad env":                 "project: {name: x}\nenv: [NOEQUALS]\n",
	}
	for name, yml := range cases {
		if _, err := load(yml); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDeprecatedGoReleaserNames(t *testing.T) {
	cfg, err := load("project: {name: x}\nbrews: [{tap: {owner: o, name: homebrew-tap}, folder: Formula}]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Brews[0].Repository.Owner != "o" || cfg.Brews[0].Directory != "Formula" || len(cfg.Warnings()) != 2 {
		t.Errorf("brew = %+v warnings = %v", cfg.Brews[0], cfg.Warnings())
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]ByteSize{"10": 10, "1KiB": 1024, "2 MiB": 2 << 20, "3GiB": 3 << 30} {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "1.5GiB", "1GB", "99999999999TiB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) accepted", in)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, ok := range []string{"0644", "0755", "644", "0400", "0555"} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("ParseMode(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"4755", "2755", "1755", "0777", "0666", "0775", "0044", "75", "0x755", ""} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) accepted", bad)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add("project: {name: x}\narchives: [{files: [a, {src: b, dst: c/}]}]\n")
	f.Add("builds: [{command: [a, b], targets: [{os: linux, arch: amd64}]}]")
	f.Fuzz(func(t *testing.T, s string) {
		cfg, err := Parse([]byte(s))
		if err != nil {
			return
		}
		cfg.Root = "/r"
		cfg.ApplyDefaults()
		_ = cfg.Validate()
	})
}

func TestBrewIDsMayNameImportingBuilds(t *testing.T) {
	ok := `project: {name: x}
builds: [{id: gr, goreleaser: {archives: true}}]
brews: [{ids: [gr]}]
`
	if _, err := load(ok); err != nil {
		t.Errorf("importing build id rejected: %v", err)
	}
	bad := `project: {name: x}
builds: [{id: gr, goreleaser: {}}]
brews: [{ids: [gr]}]
`
	if _, err := load(bad); err == nil {
		t.Error("build that imports no archives accepted as a brew id")
	}
}

// TestPullRequestBranchConflicts detects, before publishing, a pull request
// whose head branch is its base, and warns when the head branch looks like a
// default branch.
func TestPullRequestBranchConflicts(t *testing.T) {
	head := "project: {name: t}\narchives: [{id: default, targets: [{os: linux, arch: amd64}]}]\nbrews:\n  - repository:\n      owner: o\n      name: homebrew-tap\n"
	_, err := load(head + "      branch: main\n      pull_request: {base: main}\n")
	if err == nil || !strings.Contains(err.Error(), `"main" is also the pull request base`) {
		t.Errorf("err = %v", err)
	}
	cfg, err := load(head + "      branch: main\n")
	if err != nil || !strings.Contains(strings.Join(cfg.Warnings(), "\n"), `repository.branch "main" is the pull request head branch`) {
		t.Errorf("warnings %v, err %v", cfg.Warnings(), err)
	}
	cfg, err = load(head + "      branch: main\n      pull_request: {enabled: false}\n")
	if err != nil || len(cfg.Warnings()) != 0 {
		t.Errorf("direct commit to main: warnings %v, err %v", cfg.Warnings(), err)
	}
}

// TestReleaseBranches: release.github.branches are exact branch names, each
// once; a preflight.branch publish would tag outside them is warned about.
func TestReleaseBranches(t *testing.T) {
	base := "project: {name: t}\narchives: [{id: default, targets: [{os: linux, arch: amd64}]}]\nrelease:\n  github: {owner: o, name: t, branches: %s}\n"
	cfg, err := load(fmt.Sprintf(base, "[main, v1.x, release/1.2]"))
	if err != nil || strings.Join(cfg.Release.GitHub.Branches, ",") != "main,v1.x,release/1.2" || len(cfg.Warnings()) != 0 {
		t.Fatalf("cfg %+v, warnings %v, err %v", cfg, cfg.Warnings(), err)
	}
	for _, bad := range []string{`"release/*"`, `"v1.?"`, `"[ab]"`, `refs/heads/main`, `v1.2.3`, `0123abc`, `"-main"`, `"+main"`, `"a..b"`, `"a b"`, `"a~1"`, `"a^"`, `"a:b"`, `"x.lock"`, `"a/.b"`, `"a/"`, `HEAD`, `"@"`, `"a@{1}"`, `""`} {
		_, err := load(fmt.Sprintf(base, "["+bad+"]"))
		if err == nil || !strings.Contains(err.Error(), "release.github.branches[0]") {
			t.Errorf("branch %s: err = %v", bad, err)
		}
	}
	if _, err := load(fmt.Sprintf(base, "[main, main]")); err == nil || !strings.Contains(err.Error(), `"main" is listed twice`) {
		t.Errorf("duplicate: err = %v", err)
	}

	pf := "preflight: {branch: %s}\n"
	cfg, err = load(fmt.Sprintf(base, "[main, v1.x]") + fmt.Sprintf(pf, "v1.x"))
	if err != nil || len(cfg.Warnings()) != 0 {
		t.Errorf("preflight.branch in the list: warnings %v, err %v", cfg.Warnings(), err)
	}
	cfg, err = load(fmt.Sprintf(base, "[main]") + fmt.Sprintf(pf, "v1.x"))
	if err != nil || !strings.Contains(strings.Join(cfg.Warnings(), "\n"), `preflight.branch "v1.x" is not in release.github.branches`) {
		t.Errorf("preflight.branch outside the list: warnings %v, err %v", cfg.Warnings(), err)
	}
	cfg, err = load(fmt.Sprintf(base, "[]") + fmt.Sprintf(pf, "v1.x"))
	if err != nil || !strings.Contains(strings.Join(cfg.Warnings(), "\n"), "unless \"v1.x\" is the default branch") {
		t.Errorf("preflight.branch, default branch only: warnings %v, err %v", cfg.Warnings(), err)
	}
}
