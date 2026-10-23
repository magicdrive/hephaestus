package goreleaser

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

func load(t *testing.T, name string) (string, []config.Brew, []string, error) {
	t.Helper()
	return LoadBrews(filepath.Join("testdata", name))
}

func wantErrs(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q", subs)
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error does not contain %q:\n%v", s, err)
		}
	}
}

func joined(ws []string) string { return strings.Join(ws, "\n") }

func TestLoadBrewsValid(t *testing.T) {
	project, brews, warnings, err := load(t, "valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if project != "mycli" || len(brews) != 2 {
		t.Fatalf("project %q, %d brews", project, len(brews))
	}
	b := brews[0]
	if b.Name != "mycli" || b.Directory != "Formula" || b.Goarm != "7" || b.SkipUpload != "auto" ||
		b.Repository.Owner != "acme" || b.Repository.PullRequest == nil || b.Repository.PullRequest.Base == nil ||
		b.Repository.PullRequest.Base.Branch != "main" || len(b.Dependencies) != 3 || b.Dependencies[0].Name != "git" ||
		b.Dependencies[1].Type != "build" || b.Dependencies[2].OS != "mac" || b.CommitAuthor.Email != "release-bot@example.com" {
		t.Errorf("brews[0] = %+v", b)
	}
	if base := brews[1].Repository.PullRequest.Base; base == nil || base.Owner != "upstream" || base.Branch != "master" {
		t.Errorf("brews[1] base = %+v", base)
	}
	w := joined(warnings)
	if !strings.Contains(w, "brews: is deprecated in GoReleaser") {
		t.Errorf("missing brews deprecation warning:\n%s", w)
	}
	if strings.Contains(w, "no version key") {
		t.Errorf("unexpected version warning:\n%s", w)
	}

	// The entries are valid Hephaestus configuration.
	cfg := &config.Config{Project: config.Project{Name: project}, Brews: brews, Archives: []config.Archive{{ID: "default"}}}
	cfg.ApplyDefaults()
	if len(cfg.Warnings()) != 0 {
		t.Errorf("config warnings: %v", cfg.Warnings())
	}
	if err := cfg.Validate(); err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			if strings.Contains(e, "brews[") {
				t.Errorf("config validation: %s", e)
			}
		}
	}
}

func TestLoadBrewsGeneratesFormulae(t *testing.T) {
	project, brews, _, err := load(t, "valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Project: config.Project{Name: project}, Brews: brews}
	cfg.ApplyDefaults()
	m := manifest.New(project, "1.2.3", "v1.2.3", "")
	for _, t := range [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		name := "mycli_1.2.3_" + t[0] + "_" + t[1] + ".tar.gz"
		m.Artifacts = append(m.Artifacts, manifest.Artifact{
			Kind: manifest.KindArchive, ID: "default", Name: name, Path: "dist/" + name, OS: t[0], Arch: t[1],
			Format: "tar.gz", SHA256: strings.Repeat("a", 64), Source: manifest.Source{Stage: "archive"},
			Executables: []string{"mycli"},
			Files: []manifest.File{
				{Path: "mycli", Type: "file", Mode: "0755"},
				{Path: "manpages", Type: "dir", Mode: "0755"},
				{Path: "manpages/mycli.1.gz", Type: "file", Mode: "0644"},
			},
		})
	}
	vars := tmpl.Vars{Project: project, ProjectName: project, Version: "1.2.3", Tag: "v1.2.3", Major: "1", Minor: "2", Patch: "3"}
	fs, err := homebrew.Generate(homebrew.Input{Config: cfg, Manifest: m, Vars: vars, Owner: "acme", Repo: "mycli", Tag: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Path != "dist/homebrew/Formula/mycli.rb" || fs[1].ClassName != "MycliAT1" {
		t.Fatalf("formulae = %+v", fs)
	}
	c := string(fs[0].Content)
	for _, want := range []string{
		`url "https://github.com/acme/mycli/releases/download/v1.2.3/mycli_1.2.3_darwin_arm64.tar.gz"`,
		`man1.install "manpages/mycli.1.gz"`,
		`depends_on "go" => :build`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in\n%s", want, c)
		}
	}
	if ruby, err := exec.LookPath("ruby"); err == nil {
		for _, f := range fs {
			p := filepath.Join(t.TempDir(), f.Name+".rb")
			if err := os.WriteFile(p, f.Content, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(ruby, "-c", p).CombinedOutput(); err != nil {
				t.Errorf("ruby -c %s: %v\n%s", f.Name, err, out)
			}
		}
	}
	pub, err := homebrew.ResolvePublish(&cfg.Brews[0], vars)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.PullRequest || pub.Branch != "mycli-1.2.3" || pub.BaseBranch != "main" || pub.Path != "Formula/mycli.rb" {
		t.Errorf("publish = %+v", pub)
	}
}

func TestLoadBrewsPro(t *testing.T) {
	_, _, _, err := load(t, "pro.yaml")
	wantErrs(t, err,
		"brews[0].alternative_names: is a GoReleaser Pro only field, not supported",
		"brews[0].app: is a GoReleaser Pro only field",
		"brews[0].repository.token_type: is a GoReleaser Pro only field",
		"brews[0].repository.pull_request.check_boxes: is a GoReleaser Pro only field",
		"line 5:",
	)
}

func TestLoadBrewsUnknown(t *testing.T) {
	_, _, _, err := load(t, "unknown.yaml")
	wantErrs(t, err,
		`brews[0].descriptoin: unknown field (did you mean "description"?)`,
		`brews[0].repository.nmae: unknown field (did you mean "name"?)`,
		`brews[0].dependencies[0].typ: unknown field (did you mean "type"?)`,
	)
}

func TestLoadBrewsRemoved(t *testing.T) {
	_, _, warnings, err := load(t, "removed.yaml")
	wantErrs(t, err, "brews[1].plist: was removed in GoReleaser v2")
	w := joined(warnings)
	for _, s := range []string{"no version key", "brews[0].folder: was removed in GoReleaser v2", "brews[0].tap: was removed in GoReleaser v2"} {
		if !strings.Contains(w, s) {
			t.Errorf("missing warning %q:\n%s", s, w)
		}
	}

	// Without plist the removed names are converted.
	dir := t.TempDir()
	p := filepath.Join(dir, ".goreleaser.yaml")
	data, _ := os.ReadFile(filepath.Join("testdata", "removed.yaml"))
	data = []byte(strings.Split(string(data), "  - name: other")[0])
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, brews, _, err := LoadBrews(p)
	if err != nil {
		t.Fatal(err)
	}
	if b := brews[0]; b.Directory != "Formula" || b.Folder != "" || b.Tap != nil || b.Repository.Name != "homebrew-tap" {
		t.Errorf("not converted: %+v", b)
	}
}

func TestLoadBrewsTemplates(t *testing.T) {
	_, _, _, err := load(t, "templates.yaml")
	wantErrs(t, err,
		"brews[0].description: template is not supported by Hephaestus",
		"Summary",
		"brews[0].homepage: template is not supported",
		"envOrDefault",
		"brews[0].install: template is not supported",
		"Binary",
		"brews[0].license: templates are not supported here",
	)
	if strings.Contains(err.Error(), "brews[0].test") {
		t.Errorf("supported template rejected: %v", err)
	}
}

func TestLoadBrewsUnsupported(t *testing.T) {
	_, _, warnings, err := load(t, "unsupported.yaml")
	wantErrs(t, err,
		"brews[0].commit_author.signing: is not supported",
		"brews[0].repository.git: is not supported",
		"brews[0].repository.pull_request.body: is not supported",
	)
	if !strings.Contains(joined(warnings), "use_github_app_token: has no effect") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestLoadBrewsInline(t *testing.T) {
	cases := []struct{ yaml, err, warn string }{
		{"project_name: x\n", "", "no version key"},
		{"version: 1\nproject_name: x\n", "", "version 1"},
		{"version: 2\nbrews: {}\n", "brews: must be a list", ""},
		{"version: 2\nbrews:\n  - just-a-string\n", "must be a mapping", ""},
		{"version: 2\nbrews:\n  - name: a\n    repository: acme/tap\n", "expected a mapping", ""},
		{"version: 2\nbrews:\n  - name: a\n    name: b\n", "duplicate key", ""},
		{"version: 2\nbrews:\n  - &x\n    name: a\n  - *x\n", "aliases", ""},
		{"version: 2\nproject_name: \"{{ .Env.X }}\"\n", "templates are not supported in project_name", ""},
		{"version: 2\nbrews:\n  - name: a\n    install: bin.install \"a_{{ .Os }}\"\n", "", "per-artifact variable .Os"},
		{"version: 2\nbrews:\n  - name: a\n    tap: {owner: a, name: b}\n    repository: {owner: a, name: b}\n", "cannot be combined", ""},
		{"version: 2\nhomebrew_casks:\n  - name: a\n", "", "homebrew_casks: is ignored"},
		{"", "empty", ""},
		{"a: 1\n---\nb: 2\n", "single YAML document", ""},
		{"- 1\n", "must be a mapping", ""},
		{"version: 2\nbrews:\n  - name: a\n    dependencies:\n      - name: \"{{ .Env.DEP }}\"\n", "templates are not supported here", ""},
	}
	for _, tc := range cases {
		p := filepath.Join(t.TempDir(), "g.yaml")
		if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, warnings, err := LoadBrews(p)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.yaml, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%q: err = %v, want %q", tc.yaml, err, tc.err)
		}
		if tc.warn != "" && !strings.Contains(joined(warnings), tc.warn) {
			t.Errorf("%q: warnings %v, want %q", tc.yaml, warnings, tc.warn)
		}
	}
	if _, _, _, err := LoadBrews(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}
