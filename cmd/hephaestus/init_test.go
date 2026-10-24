package hephaestus

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitTemplatesProduceReleases: each template, written into a project
// of its kind, loads, and (where no foreign toolchain is needed) actually
// produces archives, checksums and a formula.
func TestInitTemplatesProduceReleases(t *testing.T) {
	clearGitHubEnv(t)
	for _, tc := range []struct {
		kind   string
		files  map[string]string
		stages string // empty: check only
		want   []string
	}{
		{"script", map[string]string{"mycli": "#!/bin/sh\necho mycli\n", "README.md": "# mycli\n"}, "archive,checksum,formula",
			[]string{"mycli_1.0.0_noarch.tar.gz", "mycli_checksums.txt", "homebrew/mycli.rb"}},
		{"prebuilt", map[string]string{
			"prebuilt/darwin_amd64/mycli": "bin", "prebuilt/darwin_arm64/mycli": "bin",
			"prebuilt/linux_amd64/mycli": "bin", "prebuilt/linux_arm64/mycli": "bin", "LICENSE": "MIT\n"},
			"build,archive,checksum,formula",
			[]string{"mycli_1.0.0_darwin_arm64.tar.gz", "mycli_1.0.0_linux_amd64.tar.gz", "mycli_checksums.txt", "homebrew/mycli.rb"}},
		{"go", map[string]string{"go.mod": "module example.com/mycli\n\ngo 1.22\n", "main.go": "package main\n\nvar version = \"dev\"\n\nfunc main() { println(version) }\n"}, "build,archive,checksum,formula",
			[]string{"mycli_1.0.0_windows_amd64.zip", "mycli_1.0.0_linux_arm64.tar.gz", "homebrew/mycli.rb"}},
		{"rust", map[string]string{"Cargo.toml": "[package]\nname = \"mycli\"\n"}, "", nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "mycli")
			for name, content := range tc.files {
				p := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cfg := filepath.Join(dir, "hephaestus.yaml")
			if code, out, errs := runCLI(t, "init", "--template", tc.kind, "--repo", "acme/mycli", "--output", cfg); code != 0 || !strings.Contains(out, "wrote") {
				t.Fatalf("init: %d\n%s%s", code, out, errs)
			}
			if code, out, errs := runCLI(t, "check", "--config", cfg); code != 0 {
				t.Fatalf("check: %d\n%s%s", code, out, errs)
			}
			if tc.stages == "" {
				return
			}
			if tc.kind == "go" {
				if testing.Short() {
					t.Skip("cross-compiles six targets")
				}
				if _, err := exec.LookPath("go"); err != nil {
					t.Skip("go is not installed")
				}
			}
			if tc.kind != "go" {
				// The next step init prints: a snapshot of every configured
				// stage that needs no tag (no formula, release or tap).
				if code, out, errs := runCLI(t, "run", "--config", cfg, "--snapshot"); code != 0 {
					t.Fatalf("run --snapshot: %d\n%s%s", code, out, errs)
				}
				if _, err := os.Stat(filepath.Join(dir, "dist", "homebrew", "mycli.rb")); err == nil {
					t.Error("run --snapshot generated a formula")
				}
			}
			if code, out, errs := runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0", "--fresh", "--stages", tc.stages); code != 0 {
				t.Fatalf("run: %d\n%s%s", code, out, errs)
			}
			for _, name := range tc.want {
				if _, err := os.Stat(filepath.Join(dir, "dist", filepath.FromSlash(name))); err != nil {
					t.Errorf("missing %s: %v", name, err)
				}
			}
			formula, _ := os.ReadFile(filepath.Join(dir, "dist", "homebrew", "mycli.rb"))
			if !strings.Contains(string(formula), `bin.install "mycli"`) || !strings.Contains(string(formula), `system bin/"mycli", "--version"`) || !strings.Contains(string(formula), "https://github.com/acme/mycli/releases/download/v1.0.0/") {
				t.Errorf("formula:\n%s", formula)
			}
		})
	}
}

func TestInitNeverOverwrites(t *testing.T) {
	clearGitHubEnv(t)
	cfg := filepath.Join(t.TempDir(), "hephaestus.yaml")
	if err := os.WriteFile(cfg, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCLI(t, "init", "--template", "go", "--name", "x", "--repo", "a/b", "--output", cfg)
	if data, _ := os.ReadFile(cfg); code != 1 || string(data) != "keep\n" || !strings.Contains(errs, "never overwritten") {
		t.Fatalf("code %d, %q, %s", code, data, errs)
	}
}

func TestInitUsage(t *testing.T) {
	clearGitHubEnv(t)
	out := filepath.Join(t.TempDir(), "h.yaml")
	for _, args := range [][]string{
		{"init"},
		{"init", "--template", "cobol", "--repo", "a/b"},
		{"init", "--template", "go", "--name", "x", "--output", out},                    // no repository
		{"init", "--template", "go", "--name", "x", "--repo", "a/b/c", "--output", out}, // bad repository
		{"init", "--template", "go", "--name", "Bad Name", "--repo", "a/b", "--output", out},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a usage error wrote a file")
	}
}
