package main

// Consistency tests for the self-release path: `make publish` and the
// release workflow steps that internal/publish relies on.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/publish"
)

// TestReleaseWorkflow_TapTokenOnlyWhenTapPublished: the tap token reaches
// only steps that run when the Go plan says the tap is published, so a
// prerelease never has it in its environment; the steps publish verifies
// by name exist.
func TestReleaseWorkflow_TapTokenOnlyWhenTapPublished(t *testing.T) {
	rel := readWorkflow(t, ".github/workflows/release.yml").Jobs["release"]
	if rel == nil || rel.Environment != publish.ReleaseEnvironment {
		t.Fatalf("release job must run in environment %q", publish.ReleaseEnvironment)
	}
	plan := -1
	var names []string
	for i, s := range rel.Steps {
		names = append(names, s.Name)
		if s.ID == "plan" {
			plan = i
			if !strings.Contains(s.Run, "go run ./internal/cmd/publish plan") || !strings.Contains(s.Run, "$GITHUB_OUTPUT") ||
				!strings.Contains(s.Run, "pipefail") || !strings.Contains(s.Run, "tap=(publish|skip)") {
				t.Errorf("plan step must write a checked internal/cmd/publish plan to $GITHUB_OUTPUT with pipefail: %q", s.Run)
			}
		}
		token := false
		for _, v := range s.Env {
			if strings.Contains(v, "secrets."+publish.TapTokenSecret) {
				token = true
			}
		}
		if !token {
			continue
		}
		if plan < 0 || plan > i {
			t.Errorf("step %q uses the tap token before the plan step", s.Name)
		}
		if strings.TrimSpace(s.If) != "steps.plan.outputs.tap == 'publish'" {
			t.Errorf("step %q gets the tap token with if %q; want steps.plan.outputs.tap == 'publish'", s.Name, s.If)
		}
	}
	if plan < 0 {
		t.Fatal("release job has no plan step (id: plan)")
	}
	for _, n := range []string{publish.TapPreflightStep, publish.TapPublishStep} {
		if !slices.Contains(names, n) {
			t.Errorf("release job has no step %q (internal/publish verifies it by name)", n)
		}
	}
	for _, f := range []string{publish.ReleaseWorkflowFile, publish.CIWorkflowFile} {
		if _, err := os.Stat(filepath.Join(".github/workflows", f)); err != nil {
			t.Error(err)
		}
	}
	if !strings.Contains(readFile(t, ".github/workflows/ci.yml"), "  "+publish.RequiredCheck+":") {
		t.Errorf("ci.yml has no %s job", publish.RequiredCheck)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMakefile_Publish drives the publish targets with the publish tool
// replaced by a stub that records how it was called: argument handling,
// make -n and make -j, without any git or GitHub access.
func TestMakefile_Publish(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Makefile targets macOS and Linux")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	stub := filepath.Join(dir, "stub")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$*\" \"$HEPHAESTUS_PUBLISH_ARGS\" \"$HEPHAESTUS_PUBLISH_VERSION\" \"$HEPHAESTUS_PUBLISH_TAG\" >> " + log + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(env []string, args ...string) (string, []string, error) {
		os.Remove(log)
		cmd := exec.Command("make", append([]string{"--no-print-directory", "PUBLISH=" + stub}, args...)...)
		// A clean make environment: no MAKEFLAGS or jobserver from a parent
		// `make test`, and no VERSION/YES unless the case sets them.
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			if !slices.Contains([]string{"MAKEFLAGS", "MFLAGS", "MAKELEVEL", "VERSION", "YES", "tag"}, k) && !strings.HasPrefix(k, "HEPHAESTUS_PUBLISH_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(log)
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
			if l != "" {
				lines = append(lines, l)
			}
		}
		return string(out), lines, err
	}

	for _, tc := range []struct {
		name string
		env  []string
		args []string
		want string // the one stub call: "args|ARGS|VERSION|TAG"
	}{
		{"positional", nil, []string{"publish", "v1.0.0"}, "publish -from-make|v1.0.0||"},
		{"VERSION=", nil, []string{"publish", "VERSION=v1.0.0"}, "publish -from-make||v1.0.0|"},
		{"tag=", nil, []string{"publish", "tag=v1.0.0"}, "publish -from-make|||v1.0.0"},
		{"both reach the tool, which rejects them", nil, []string{"publish", "v1.0.0", "VERSION=v1.0.0"}, "publish -from-make|v1.0.0|v1.0.0|"},
		{"no version reaches the tool, which rejects it", nil, []string{"publish"}, "publish -from-make|||"},
		{"YES=1 on the command line", nil, []string{"publish", "v1.0.0", "YES=1"}, "publish -from-make -yes|v1.0.0||"},
		{"YES and VERSION from the environment are ignored", []string{"YES=1", "VERSION=v9.9.9"}, []string{"publish", "v1.0.0"}, "publish -from-make|v1.0.0||"},
		{"check", nil, []string{"publish-check", "v1.0.0"}, "check -from-make|v1.0.0||"},
		{"status", nil, []string{"publish-status", "v1.0.0"}, "status -from-make|v1.0.0||"},
		{"watch", nil, []string{"publish-watch", "v1.0.0"}, "watch -from-make|v1.0.0||"},
		{"help", nil, []string{"publish-help"}, "help|||"},
		{"parallel make runs the tool once", nil, []string{"-j4", "publish", "v1.0.0"}, "publish -from-make|v1.0.0||"},
		// Were the goal pasted into the recipe, the shell would run false.
		{"shell metacharacters stay data", nil, []string{"publish", "v1;false"}, "publish -from-make|v1;false||"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, calls, err := mk(tc.env, tc.args...)
			if err != nil {
				t.Fatalf("make %v: %v\n%s", tc.args, err, out)
			}
			if len(calls) != 1 || calls[0] != tc.want {
				t.Fatalf("make %v called the tool %q, want once %q\n%s", tc.args, calls, tc.want, out)
			}
		})
	}

	for _, args := range [][]string{
		{"publish", "v1.0.0", "v2.0.0"},
		{"build", "publish", "v1.0.0"},
		{"publish", "publish-check", "v1.0.0"},
		{"publish-help", "v1.0.0"},
		{"publish", "clean"},
		{"publish", "vet"},
	} {
		out, calls, err := mk(nil, args...)
		if err == nil || len(calls) != 0 {
			t.Errorf("make %v: err %v, calls %q\n%s", args, err, calls, out)
		}
	}

	out, calls, err := mk(nil, "-n", "publish", "v1.0.0")
	if err != nil || len(calls) != 0 || !strings.Contains(out, "publish -from-make") {
		t.Errorf("make -n ran the tool (calls %q, err %v)\n%s", calls, err, out)
	}
}
