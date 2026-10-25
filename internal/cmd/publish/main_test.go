package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/publish"
)

func runMain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

var repoConfig = filepath.Join("..", "..", "..", "hephaestus.yaml")

// TestPlan is the release workflow's contract: key=value lines for
// $GITHUB_OUTPUT, decided from hephaestus.yaml.
func TestPlan(t *testing.T) {
	for tag, want := range map[string]string{
		"v0.1.0-rc.1": "tag=v0.1.0-rc.1\nversion=0.1.0-rc.1\nprerelease=true\ntap=skip\n",
		"v1.0.0":      "tag=v1.0.0\nversion=1.0.0\nprerelease=false\ntap=publish\n",
	} {
		code, out, errs := runMain(t, "plan", "-config", repoConfig, tag)
		if code != 0 || out != want {
			t.Errorf("plan %s: %d %q %s", tag, code, out, errs)
		}
	}
	for _, tag := range []string{"1.0.0", "v1.0.0+build.1", "v1.0", "v1.0.0-rc.", "release"} {
		if code, out, _ := runMain(t, "plan", "-config", repoConfig, tag); code != 2 || out != "" {
			t.Errorf("plan %s: code %d, out %q", tag, code, out)
		}
	}
}

func TestArguments(t *testing.T) {
	t.Setenv(envArgs, "")
	t.Setenv(envVersion, "")
	t.Setenv(envTag, "")
	if code, _, errs := runMain(t, "plan", "-config", repoConfig, "-from-make"); code != 2 || !strings.Contains(errs, "a version is required") {
		t.Errorf("no version: %d %s", code, errs)
	}
	t.Setenv(envArgs, "v1.0.0")
	if code, out, errs := runMain(t, "plan", "-config", repoConfig, "-from-make"); code != 0 || !strings.Contains(out, "tag=v1.0.0") {
		t.Errorf("from make: %d %s", code, errs)
	}
	t.Setenv(envVersion, "v1.0.0")
	if code, _, errs := runMain(t, "plan", "-config", repoConfig, "-from-make"); code != 2 || !strings.Contains(errs, "give the version once") {
		t.Errorf("positional and VERSION: %d %s", code, errs)
	}
	t.Setenv(envArgs, "v1;false")
	t.Setenv(envVersion, "")
	if code, _, _ := runMain(t, "plan", "-config", repoConfig, "-from-make"); code != 2 {
		t.Errorf("metacharacters accepted: %d", code)
	}
	if code, _, errs := runMain(t, "plan", "-config", repoConfig, "-from-make", "v1.0.0"); code != 2 || !strings.Contains(errs, "environment only") {
		t.Errorf("-from-make with an argument: %d %s", code, errs)
	}
	if code, _, _ := runMain(t, "plan", "-config", repoConfig, "v1.0.0", "v2.0.0"); code != 2 {
		t.Error("two versions accepted")
	}
	if code, _, errs := runMain(t, "deploy"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Errorf("unknown command: %d %s", code, errs)
	}
	if code, _, _ := runMain(t); code != 2 {
		t.Error("no command accepted")
	}
	if code, out, _ := runMain(t, "help"); code != 0 || !strings.Contains(out, "never moved or deleted") || !strings.Contains(out, "make publish-watch") {
		t.Errorf("help: %d %s", code, out)
	}
}

// actionRepo is a repository root with this repository's configuration and
// action/VERSION set to version ("" for no file).
func actionRepo(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	cfg, err := os.ReadFile(repoConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hephaestus.yaml"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		if err := os.MkdirAll(filepath.Join(dir, "action"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "action", "VERSION"), []byte(version), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "hephaestus.yaml")
}

// TestActionVersion: the Action downloads the release action/VERSION
// names, so make publish and make publish-check stop on another tag before
// anything else is read; status, watch and plan do not check it.
func TestActionVersion(t *testing.T) {
	for _, cmd := range []string{"publish", "check"} {
		cfg := actionRepo(t, "v1.2.0-rc.1\n")
		code, out, errs := runMain(t, cmd, "-config", cfg, "v1.2.0")
		if want := "publish: action/VERSION is v1.2.0-rc.1, not v1.2.0: set it to v1.2.0 in a pull request first\n"; code != 1 || errs != want || out != "" {
			t.Errorf("%s: %d stdout %q stderr %q", cmd, code, out, errs)
		}
	}
	// plan is not checked (the release workflow enforces action/VERSION
	// itself).
	if code, out, errs := runMain(t, "plan", "-config", actionRepo(t, "v1.2.0-rc.1\n"), "v1.2.0"); code != 0 || !strings.Contains(out, "tag=v1.2.0\n") {
		t.Errorf("plan: %d %s", code, errs)
	}
	for content, ok := range map[string]bool{
		"":                 true, // no file: not checked
		"v1.2.0\n":         true,
		"v1.2.0\r\n":       true,
		"v1.2.0":           true,
		"v1.2.0-rc.1\n":    false,
		"v1.2.0\nv1.3.0\n": false,
		" v1.2.0\n":        false,
		"\n":               false,
	} {
		v, err := publish.ParseVersion("v1.2.0")
		if err != nil {
			t.Fatal(err)
		}
		if err := actionVersionMatches(actionRepo(t, content), v); (err == nil) != ok {
			t.Errorf("%q: %v", content, err)
		}
	}
	// status and watch do not check it: with a mismatch they go on to read
	// the configuration (here: an invalid one, so nothing else runs).
	dir := filepath.Dir(actionRepo(t, "v0.0.1\n"))
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\nproject: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"status", "watch"} {
		if code, _, errs := runMain(t, cmd, "-config", bad, "v1.2.0"); code != 1 || strings.Contains(errs, "action/VERSION") {
			t.Errorf("%s: %d %s", cmd, code, errs)
		}
	}
	if code, _, errs := runMain(t, "check", "-config", bad, "v1.2.0"); code != 1 || !strings.Contains(errs, "action/VERSION is v0.0.1") {
		t.Errorf("check before the configuration: %d %s", code, errs)
	}
}
