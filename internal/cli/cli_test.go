package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

// project writes a project whose "build" uses prebuilt executables, so the
// test needs no compiler or shell.
func project(t *testing.T, extra string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"prebuilt/darwin_arm64/tool": "#!/bin/sh\necho tool\n",
		"prebuilt/linux_amd64/tool":  "#!/bin/sh\necho tool\n",
		"README.md":                  "readme\n",
		"hephaestus.yaml": `
project: {name: tool, description: Example tool for tests, homepage: "https://example.com", license: MIT}
builds:
  - id: pre
    existing: {path: "prebuilt/{{ .OS }}_{{ .Arch }}/tool"}
    targets: [{os: darwin, arch: arm64}, {os: linux, arch: amd64}]
archives:
  - files:
      - {src: "prebuilt/{{ .OS }}_{{ .Arch }}/tool", dst: bin/tool, mode: "0755"}
      - README.md
` + extra,
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func clearGitHubEnv(t *testing.T) {
	for _, k := range []string{"GITHUB_REF_TYPE", "GITHUB_REF_NAME", "GITHUB_REPOSITORY", "GITHUB_TOKEN", "HOMEBREW_TAP_GITHUB_TOKEN", "GITHUB_SHA", "SOURCE_DATE_EPOCH"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("GITHUB_OUTPUT", filepath.Join(dir, "output"))
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary"))
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, _ := runCLI(t); code != 2 {
		t.Error("no args should be a usage error")
	}
	if code, out, _ := runCLI(t, "version"); code != 0 || !strings.Contains(out, "hephaestus") {
		t.Error("version")
	}
	if code, _, errs := runCLI(t, "deploy"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Error("unknown command")
	}
}

func TestCheck(t *testing.T) {
	root := project(t, "")
	code, out, errs := runCLI(t, "check", "--config", filepath.Join(root, "hephaestus.yaml"))
	if code != 0 || !strings.Contains(out, "configured stages: build, archive, checksum") {
		t.Fatalf("check: %d %s %s", code, out, errs)
	}
	os.WriteFile(filepath.Join(root, "bad.yaml"), []byte("project: {name: x}\narchives: [{fromat: zip}]\n"), 0o644)
	code, _, errs = runCLI(t, "check", "--config", filepath.Join(root, "bad.yaml"))
	if code != 1 || !strings.Contains(errs, `did you mean "format"`) {
		t.Errorf("bad config: %d %s", code, errs)
	}
}

func TestRunLocalStagesWritesOutputs(t *testing.T) {
	clearGitHubEnv(t)
	root := project(t, "")
	cfg := filepath.Join(root, "hephaestus.yaml")
	code, _, errs := runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0")
	if code != 0 {
		t.Fatalf("run: %s", errs)
	}
	m, err := manifest.ReadFile(filepath.Join(root, "dist", "hephaestus-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Filter(manifest.KindBinary)) != 2 || len(m.Filter(manifest.KindArchive)) != 2 || len(m.Filter(manifest.KindChecksum)) != 1 {
		t.Errorf("manifest: %+v", m.Artifacts)
	}
	out, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	if !strings.Contains(string(out), "status<<") || !strings.Contains(string(out), "tool_1.0.0_linux_amd64.tar.gz") {
		t.Errorf("outputs:\n%s", out)
	}
	sum, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	if !strings.Contains(string(sum), "### Artifacts") || !strings.Contains(string(sum), "✓ archive") {
		t.Errorf("summary:\n%s", sum)
	}

	// Re-running continues from the manifest; a different version is refused.
	if code, _, errs := runCLI(t, "archive", "--config", cfg, "--tag", "v1.0.0"); code != 0 {
		t.Errorf("rerun: %s", errs)
	}
	code, _, errs = runCLI(t, "archive", "--config", cfg, "--tag", "v2.0.0")
	if code != 1 || !strings.Contains(errs, "--fresh") {
		t.Errorf("version mismatch: %d %s", code, errs)
	}
	if code, _, errs := runCLI(t, "run", "--config", cfg, "--tag", "v2.0.0", "--fresh"); code != 0 {
		t.Errorf("fresh: %s", errs)
	}
}

func TestPreflightErrors(t *testing.T) {
	clearGitHubEnv(t)
	root := project(t, "release:\n  github: {}\nbrews: [{repository: {owner: o, name: homebrew-tap}}]\n")
	cfg := filepath.Join(root, "hephaestus.yaml")

	code, _, errs := runCLI(t, "run", "--config", cfg)
	if code != 1 || !strings.Contains(errs, "a tag is required") {
		t.Errorf("no tag: %s", errs)
	}
	code, _, errs = runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0")
	for _, want := range []string{"cannot start", "repository is unknown", "GitHub token is required", "GITHUB_TOKEN cannot write"} {
		if code != 1 || !strings.Contains(errs, want) {
			t.Errorf("preflight lacks %q:\n%s", want, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "dist")); !os.IsNotExist(err) {
		t.Error("a stage ran despite failed preflight")
	}
	code, _, errs = runCLI(t, "run", "--config", cfg, "--snapshot", "--stages", "build,release")
	if code != 1 || !strings.Contains(errs, "cannot run with --snapshot") {
		t.Errorf("snapshot release: %s", errs)
	}
	code, _, errs = runCLI(t, "run", "--config", cfg, "--tag", "v1.0.0", "--stages", "tap")
	if code != 1 || !strings.Contains(errs, "no formulas") {
		t.Errorf("tap without formula: %s", errs)
	}
}

func TestDryRunReleaseNeedsNoToken(t *testing.T) {
	clearGitHubEnv(t)
	root := project(t, "release:\n  github: {owner: o, name: r}\n")
	code, _, errs := runCLI(t, "run", "--config", filepath.Join(root, "hephaestus.yaml"), "--tag", "v1.0.0", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: %s", errs)
	}
	out, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	if !strings.Contains(string(out), "dry-run") {
		t.Errorf("outputs:\n%s", out)
	}
}

func TestResolveRepo(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "env/repo")
	root := project(t, "")
	cfg, err := loadConfig(filepath.Join(root, "hephaestus.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if o, r, _ := resolveRepo(cfg, ""); o != "env" || r != "repo" {
		t.Errorf("env: %s/%s", o, r)
	}
	if o, r, _ := resolveRepo(cfg, "flag/x"); o != "flag" || r != "x" {
		t.Errorf("flag: %s/%s", o, r)
	}
	for _, bad := range []string{"noslash", "a/b/c", "/x"} {
		if _, _, err := resolveRepo(cfg, bad); err == nil {
			t.Errorf("resolveRepo(%q) accepted", bad)
		}
	}
}
