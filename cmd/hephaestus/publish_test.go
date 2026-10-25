package hephaestus

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishCommand: `hephaestus publish` arguments, exit codes and help.
// No case reaches git or GitHub.
func TestPublishCommand(t *testing.T) {
	cfg := filepath.Join("..", "..", "hephaestus.yaml")
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, tc := range []struct {
		args           []string
		code           int
		stdout, stderr string
	}{
		{[]string{"publish"}, 2, "", "usage: hephaestus publish [command] [flags] <version>"},
		{[]string{"publish", "help"}, 0, "releases <project.name>", ""},
		{[]string{"publish", "-h"}, 0, "", "-discover-timeout"},
		{[]string{"publish", "check", "--help"}, 0, "", "-from-make"},
		{[]string{"publish", "deploy"}, 2, "", `hephaestus publish: unknown command "deploy"`},
		{[]string{"publish", "check"}, 2, "", "a version is required: hephaestus publish check v1.2.3"},
		{[]string{"publish", "v1.0"}, 2, "", "not SemVer"},
		{[]string{"publish", "1.0.0"}, 2, "", "did you mean v1.0.0"},
		{[]string{"publish", "-config", missing, "v1.0.0"}, 1, "", "missing.yaml"},
		{[]string{"publish", "status", "-config", missing, "v1.0.0"}, 1, "", "missing.yaml"},
		{[]string{"publish", "plan", "-config", cfg, "v1.0.0"}, 0, "tag=v1.0.0\nversion=1.0.0\nprerelease=false\ntap=publish\n", ""},
		{[]string{"publish", "plan", "--config=" + cfg, "v1.0.0-rc.1"}, 0, "prerelease=true\ntap=skip\n", ""},
	} {
		code, out, errs := runCLI(t, tc.args...)
		if code != tc.code || !strings.Contains(out, tc.stdout) || !strings.Contains(errs, tc.stderr) {
			t.Errorf("%q: %d\nstdout %q\nstderr %q", tc.args, code, out, errs)
		}
	}
	if _, out, _ := runCLI(t, "help"); !strings.Contains(out, "\n  publish ") {
		t.Errorf("usage does not list publish:\n%s", out)
	}
}
