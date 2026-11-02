package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
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
