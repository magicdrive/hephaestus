package main

import (
	"encoding/base64"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// runMainEnv makes the test binary run the guard instead of the tests: the
// workflow tests install it as tap-version-guard.
const runMainEnv = "TAP_VERSION_GUARD_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	os.Exit(m.Run())
}

const exampleWorkflow = "../../examples/workflows/tap-version-guard.yml"

type wfStep struct {
	Name  string            `yaml:"name"`
	ID    string            `yaml:"id"`
	If    string            `yaml:"if"`
	Uses  string            `yaml:"uses"`
	Shell string            `yaml:"shell"`
	Run   string            `yaml:"run"`
	Env   map[string]string `yaml:"env"`
	With  map[string]string `yaml:"with"`
}

type wfJob struct {
	If          string            `yaml:"if"`
	Needs       string            `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []wfStep          `yaml:"steps"`
}

type workflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`
	Jobs        map[string]wfJob  `yaml:"jobs"`
}

func readExample(t *testing.T) (workflow, string) {
	t.Helper()
	data, err := os.ReadFile(exampleWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	return w, string(data)
}

func (w workflow) step(t *testing.T, job, name string) wfStep {
	t.Helper()
	for _, s := range w.Jobs[job].Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("job %s has no step %q", job, name)
	return wfStep{}
}

// TestExampleWorkflow_Hardening: what the example tap workflow must keep.
func TestExampleWorkflow_Hardening(t *testing.T) {
	w, raw := readExample(t)
	if len(w.Permissions) != 0 {
		t.Errorf("top-level permissions %v, want {} (each job names its own)", w.Permissions)
	}
	wantPerms := map[string]map[string]string{
		"version-guard":     {"contents": "read", "pull-requests": "read"},
		"after-merge":       {"contents": "read", "pull-requests": "read"},
		"after-merge-issue": {"issues": "write"},
	}
	if got := slices.Sorted(maps.Keys(w.Jobs)); !slices.Equal(got, slices.Sorted(maps.Keys(wantPerms))) {
		t.Errorf("jobs %v", got)
	}
	for job, want := range wantPerms {
		if !maps.Equal(w.Jobs[job].Permissions, want) {
			t.Errorf("%s: permissions %v, want %v", job, w.Jobs[job].Permissions, want)
		}
	}
	types, _ := w.On["pull_request"].(map[string]any)["types"].([]any)
	for _, ty := range []string{"opened", "synchronize", "reopened", "edited", "labeled", "unlabeled"} {
		if !slices.Contains(types, any(ty)) {
			t.Errorf("pull_request does not run on %s", ty)
		}
	}
	if !strings.HasPrefix(w.Env["GUARD"], "github.com/magicdrive/hephaestus/cmd/tap-version-guard@") || w.Env["GOTOOLCHAIN"] != "local" {
		t.Errorf("env %v", w.Env)
	}
	pinned := regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)
	for name, job := range w.Jobs {
		for _, s := range job.Steps {
			where := name + "/" + s.Name + s.Uses
			if s.Uses != "" {
				if !pinned.MatchString(s.Uses) || !regexp.MustCompile(regexp.QuoteMeta(s.Uses)+` # v\d`).MatchString(raw) {
					t.Errorf("%s: not pinned to a commit SHA with a # vX.Y.Z comment", where)
				}
				if name == "after-merge-issue" {
					t.Errorf("%s: the job that writes issues runs no action", where)
				}
			}
			if strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["persist-credentials"] != "false" {
				t.Errorf("%s: persist-credentials must be false", where)
			}
			if strings.HasPrefix(s.Uses, "actions/setup-go@") {
				if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(s.With["go-version"]) || s.With["cache"] != "false" {
					t.Errorf("%s: with %v, want an exact go-version and cache false", where, s.With)
				}
			}
			if s.Run == "" {
				continue
			}
			if s.Shell != "bash" || !strings.HasPrefix(s.Run, "set -euo pipefail\n") {
				t.Errorf("%s: shell %q, run must start with set -euo pipefail", where, s.Shell)
			}
			if strings.Contains(s.Run, "${{") {
				t.Errorf("%s: an expression inside run (pass it through env)", where)
			}
			if strings.Contains(s.Run, `"$guard"`) {
				for _, line := range strings.Split(s.Run, "\n") {
					if strings.Contains(line, "flags=(") && !strings.Contains(line, "flags=(-strict)") {
						t.Errorf("%s: %s: the guard runs with -strict", where, strings.TrimSpace(line))
					}
				}
				if !strings.Contains(s.Run, "flags=(-strict)") {
					t.Errorf("%s: the guard runs without -strict", where)
				}
			}
			if name == "after-merge-issue" && (strings.Contains(s.Run, "GUARD") || strings.Contains(s.Run, "git ")) {
				t.Errorf("%s: the job that writes issues runs no guard and no git", where)
			}
		}
	}
	if j := w.Jobs["after-merge-issue"]; j.Needs != "after-merge" || j.If != "always() && needs.after-merge.outputs.found == '1'" {
		t.Errorf("after-merge-issue: needs %q if %q", j.Needs, j.If)
	}
}

// stubs puts fake gh and go on PATH. gh answers from files in dir (and
// fails where a *.fail file exists); go install installs this test binary
// as the guard.
func stubs(t *testing.T) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the workflow runs on Linux")
	}
	for _, tool := range []string{"bash", "git", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	bin, dir = t.TempDir(), t.TempDir()
	gh := `#!/usr/bin/env bash
set -u
echo "gh $*" >> "$STUB_DIR/calls"
case "$1 ${2:-}" in
"api --paginate")
  [ -e "$STUB_DIR/labels.fail" ] && { echo "HTTP 502" >&2; exit 1; }
  cat "$STUB_DIR/labels" ;;
"api repos/"*/pulls)
  [ -e "$STUB_DIR/pulls.fail" ] && { echo "HTTP 502" >&2; exit 1; }
  sha="${2#*/commits/}"; sha="${sha%/pulls}"
  if [ -e "$STUB_DIR/pulls-$sha.json" ]; then cat "$STUB_DIR/pulls-$sha.json"; else echo '[]'; fi ;;
"pr list")
  [ -e "$STUB_DIR/prs.fail" ] && { echo "HTTP 502" >&2; exit 1; }
  cat "$STUB_DIR/prs" 2>/dev/null || true ;;
*) echo "unexpected gh $*" >&2; exit 3 ;;
esac
`
	goStub := `#!/usr/bin/env bash
set -eu
[ "$1" = install ] || { echo "unexpected go $*" >&2; exit 3; }
mkdir -p "$GOBIN"
printf '#!/bin/sh\n` + runMainEnv + `=1 exec "%s" "$@"\n' "$GUARD_BINARY" > "$GOBIN/tap-version-guard"
chmod +x "$GOBIN/tap-version-guard"
`
	for name, body := range map[string]string{"gh": gh, "go": goStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, dir
}

// runStep runs a step's script as Actions would (bash, in dir) with env
// and returns its output and the outputs it set.
func runStep(t *testing.T, s wfStep, dir, bin, stub string, env map[string]string) (string, map[string]string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "step.sh")
	if err = os.WriteFile(script, []byte(s.Run), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "output")
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", script)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp, "STUB_DIR=" + stub, "GUARD_BINARY=" + self,
		"RUNNER_TEMP=" + tmp, "GITHUB_OUTPUT=" + out, "GITHUB_STEP_SUMMARY=" + filepath.Join(tmp, "summary"),
		"GIT_CONFIG_NOSYSTEM=1",
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("step %q: %v\n%s", s.Name, err, b)
	}
	outputs := map[string]string{}
	data, _ := os.ReadFile(out)
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			outputs[k] = v
		}
	}
	return string(b), outputs
}

func envKeys(t *testing.T, s wfStep, want ...string) {
	t.Helper()
	if got := slices.Sorted(maps.Keys(s.Env)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("step %q env %v, the test sets %v", s.Name, got, want)
	}
}

// TestExampleWorkflow_RollbackLabel: the label allows a rollback only when
// it is read; anything else (no label, an API failure, an unknown queue
// entry) checks without it.
func TestExampleWorkflow_RollbackLabel(t *testing.T) {
	w, _ := readExample(t)
	s := w.step(t, "version-guard", "Rollback label")
	envKeys(t, s, "EVENT", "PR_ROLLBACK", "QUEUE_REF", "GH_TOKEN", "REPO")
	const queueRef = "gh-readonly-queue/main/pr-12-0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name               string
		event, pr, queue   string
		labels             string // "" none; "fail": the API fails
		want               string
		wantCall, wantWarn bool
	}{
		{"pull request with the label", "pull_request", "true", "", "", "true", false, false},
		{"pull request without it", "pull_request", "false", "", "", "false", false, false},
		{"pull request, empty value", "pull_request", "", "", "", "false", false, false},
		{"queue entry with the label", "merge_group", "", queueRef, "bug\nformula-rollback\n", "true", true, false},
		{"queue entry without it", "merge_group", "", queueRef, "formula-rollback-later\nbug\n", "false", true, false},
		{"queue entry, labels unreadable", "merge_group", "", queueRef, "fail", "false", true, true},
		{"queue entry, unknown ref", "merge_group", "", "gh-readonly-queue/main/weird", "formula-rollback\n", "false", false, true},
		{"queue entry, crafted ref", "merge_group", "", "x$(touch pwned)`touch pwned`/pr-1-ab", "formula-rollback\n", "false", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, stub := stubs(t)
			if tc.labels == "fail" {
				os.WriteFile(filepath.Join(stub, "labels.fail"), nil, 0o644)
			} else {
				os.WriteFile(filepath.Join(stub, "labels"), []byte(tc.labels), 0o644)
			}
			dir := t.TempDir()
			out, outputs := runStep(t, s, dir, bin, stub, map[string]string{
				"EVENT": tc.event, "PR_ROLLBACK": tc.pr, "QUEUE_REF": tc.queue, "GH_TOKEN": "t", "REPO": "o/tap",
			})
			if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
				t.Fatal("the queue ref ran a command")
			}
			if outputs["allow"] != tc.want {
				t.Fatalf("allow=%q, want %q\n%s", outputs["allow"], tc.want, out)
			}
			calls, _ := os.ReadFile(filepath.Join(stub, "calls"))
			if (len(calls) > 0) != tc.wantCall || (tc.wantCall && !strings.Contains(string(calls), "repos/o/tap/issues/12/labels")) {
				t.Fatalf("gh calls %q", calls)
			}
			if strings.Contains(out, "::warning::") != tc.wantWarn {
				t.Fatalf("warning: %s", out)
			}
		})
	}
}

// afterMerge is a tap on a local "origin" (bare) with release branches
// pr-a (2.0.0), pr-b (1.5.0) and pr-c (a malformed formula) opened on
// main at 1.0.0.
type afterMerge struct {
	*tapRepo
	origin, oldBase string
}

func newAfterMerge(t *testing.T) *afterMerge {
	r := newTap(t)
	r.commit(map[string]string{"tool.rb": brew("1.0.0"), "README.md": "tap"})
	a := &afterMerge{tapRepo: r, origin: filepath.Join(t.TempDir(), "origin.git"), oldBase: r.git("rev-parse", "main")}
	for _, pr := range []struct{ branch, content string }{{"pr-a", brew("2.0.0")}, {"pr-b", brew("1.5.0")}, {"pr-c", "garbage\n"}} {
		r.git("checkout", "-q", "-b", pr.branch, "main")
		r.commit(map[string]string{"tool.rb": pr.content})
	}
	r.git("checkout", "-q", "main")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", a.origin).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return a
}

func (a *afterMerge) merge(branch string) string {
	a.t.Helper()
	if err := exec.Command("git", "-C", a.dir, "merge", "-q", "-X", "theirs", "--no-ff", "--no-edit", branch).Run(); err != nil {
		a.t.Fatal(err)
	}
	return a.git("rev-parse", "HEAD")
}

// publish pushes main and the pull request heads (refs/pull/<n>/head) and
// returns a fresh clone, as actions/checkout with fetch-depth 0 leaves it.
func (a *afterMerge) publish(pulls map[int]string) string {
	a.t.Helper()
	refs := []string{"push", "-q", "--force", a.origin, "main:refs/heads/main"}
	for n, b := range pulls {
		refs = append(refs, b+":refs/pull/"+strconv.Itoa(n)+"/head")
	}
	a.git(refs...)
	work := filepath.Join(a.t.TempDir(), "work")
	if out, err := exec.Command("git", "clone", "-q", a.origin, work).CombinedOutput(); err != nil {
		a.t.Fatalf("%v\n%s", err, out)
	}
	return work
}

// TestExampleWorkflow_AfterMerge runs the after-merge job's check on local
// repositories: what lands or would land refused opens the issue; what
// could not be checked fails the job without one; a rollback label of the
// pull request is honoured, and a failed read of it is not a label.
func TestExampleWorkflow_AfterMerge(t *testing.T) {
	w, _ := readExample(t)
	s := w.step(t, "after-merge", "Check what landed and the open pull requests")
	envKeys(t, s, "EVENT", "BEFORE", "AFTER", "BRANCH", "GH_TOKEN", "REPO", "GIT_TERMINAL_PROMPT")
	type result struct {
		found, errors string
		report, out   string
	}
	run := func(t *testing.T, a *afterMerge, work, event, before string, stub, bin string) result {
		t.Helper()
		out, o := runStep(t, s, work, bin, stub, map[string]string{
			"EVENT": event, "BEFORE": before, "AFTER": a.git("rev-parse", "main"), "BRANCH": "main",
			"GH_TOKEN": "t", "REPO": "o/tap", "GIT_TERMINAL_PROMPT": "0",
			"GUARD": w.Env["GUARD"], "GOTOOLCHAIN": "local",
		})
		rep, err := base64.StdEncoding.DecodeString(o["report"])
		if err != nil {
			t.Fatalf("report output: %v", err)
		}
		return result{o["found"], o["errors"], string(rep), out}
	}
	write := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("A then B pushed: B's merge is reported", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		mergeB := a.merge("pr-b")
		got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin)
		if got.found != "1" || got.errors != "0" || !strings.Contains(got.report, "### "+mergeB) ||
			!strings.Contains(got.report, "tool.rb: version goes down from 2.0.0 to 1.5.0") || strings.Count(got.report, "### ") != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("A then B pushed, B labelled formula-rollback", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		mergeB := a.merge("pr-b")
		write(t, stub, "pulls-"+mergeB+".json", `[{"number":2,"merge_commit_sha":"`+mergeB+`","merged_at":"2026-10-27T00:00:00Z","labels":[{"name":"formula-rollback"}]}]`)
		got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin)
		if got.found != "0" || got.errors != "0" || !strings.Contains(got.out, "(allowed: -allow-downgrade)") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("A then B pushed, the label of another commit does not count", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		mergeA := a.merge("pr-a")
		mergeB := a.merge("pr-b")
		write(t, stub, "pulls-"+mergeB+".json", `[{"number":2,"merge_commit_sha":"`+mergeA+`","merged_at":"2026-10-27T00:00:00Z","labels":[{"name":"formula-rollback"}]}]`)
		if got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin); got.found != "1" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("labels unreadable: no rollback", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		a.merge("pr-b")
		write(t, stub, "pulls.fail", "")
		got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin)
		if got.found != "1" || !strings.Contains(got.out, "::warning::cannot read the pull request") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("A merged: the open B is reported, the malformed C is not", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		work := a.publish(map[int]string{2: "pr-b", 3: "pr-c"})
		write(t, stub, "prs", "2 "+a.git("rev-parse", "pr-b")+" false\n3 "+a.git("rev-parse", "pr-c")+" false\n")
		got := run(t, a, work, "push", a.oldBase, stub, bin)
		if got.found != "1" || got.errors != "0" || !strings.Contains(got.report, "### #2 would be refused") ||
			!strings.Contains(got.report, "goes down from 2.0.0 to 1.5.0") || strings.Contains(got.report, "#3") ||
			!strings.Contains(got.out, "::notice::pull request #3 fails its own check too") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("workflow_dispatch re-checks the open pull requests", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		work := a.publish(map[int]string{2: "pr-b"})
		write(t, stub, "prs", "2 "+a.git("rev-parse", "pr-b")+" false\n")
		if got := run(t, a, work, "workflow_dispatch", "", stub, bin); got.found != "1" || !strings.Contains(got.report, "#2") {
			t.Fatalf("%+v", got)
		}
		write(t, stub, "prs", "2 "+a.git("rev-parse", "pr-b")+" true\n")
		if got := run(t, a, work, "workflow_dispatch", "", stub, bin); got.found != "0" || got.errors != "0" {
			t.Fatalf("labelled: %+v", got)
		}
	})
	t.Run("pull requests unreadable: the job fails, no issue", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		write(t, stub, "prs.fail", "")
		got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin)
		if got.found != "0" || got.errors != "1" || !strings.Contains(got.out, "::error::cannot list the open pull requests") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("pull request head missing: the job fails, no issue", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		write(t, stub, "prs", "9 "+a.git("rev-parse", "pr-b")+" false\n")
		got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin)
		if got.found != "0" || got.errors != "1" || !strings.Contains(got.out, "::error::cannot fetch pull request #9") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a clean push opens nothing", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		if got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin); got.found != "0" || got.errors != "0" || got.report != "" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("forced push: the last commit only", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		a.merge("pr-a")
		a.merge("pr-b")
		got := run(t, a, a.publish(nil), "push", strings.Repeat("e", 40), stub, bin)
		if got.found != "1" || !strings.Contains(got.out, "::warning::no fast-forward") {
			t.Fatalf("%+v", got)
		}
	})
}
