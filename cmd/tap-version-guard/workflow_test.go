package main

import (
	"encoding/base64"
	"encoding/json"
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
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []wfStep          `yaml:"steps"`
}

type workflow struct {
	On          map[string]any    `yaml:"on"`
	Concurrency map[string]any    `yaml:"concurrency"`
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
		"version-guard-pending": {"statuses": "write"},
		"version-guard":         {"contents": "read", "pull-requests": "read"},
		"version-guard-verdict": {"statuses": "write"},
		"after-merge":           {"contents": "read", "pull-requests": "read"},
		"after-merge-issue":     {"issues": "write"},
	}
	if got := slices.Sorted(maps.Keys(w.Jobs)); !slices.Equal(got, slices.Sorted(maps.Keys(wantPerms))) {
		t.Errorf("jobs %v", got)
	}
	for job, want := range wantPerms {
		if !maps.Equal(w.Jobs[job].Permissions, want) {
			t.Errorf("%s: permissions %v, want %v", job, w.Jobs[job].Permissions, want)
		}
	}
	// The required check runs the workflow of the default branch: a
	// pull_request trigger would run the one the pull request brings.
	if _, ok := w.On["pull_request"]; ok {
		t.Error("on: pull_request runs the workflow of the pull request; use pull_request_target")
	}
	if _, ok := w.On["pull_request_review"]; ok {
		t.Error("on: pull_request_review runs the workflow of the pull request")
	}
	prt, _ := w.On["pull_request_target"].(map[string]any)
	types, _ := prt["types"].([]any)
	for _, ty := range []string{"opened", "synchronize", "reopened", "edited", "labeled", "unlabeled"} {
		if !slices.Contains(types, any(ty)) {
			t.Errorf("pull_request_target does not run on %s", ty)
		}
	}
	if len(prt) != 1 {
		t.Errorf("pull_request_target %v: no branch or path filter (every pull request is checked)", prt)
	}
	if g, _ := w.Concurrency["group"].(string); !strings.Contains(g, "github.event.pull_request.number") ||
		w.Concurrency["cancel-in-progress"] != "${{ github.event_name == 'pull_request_target' }}" {
		t.Errorf("concurrency %v: one verdict per pull request, the newest event wins", w.Concurrency)
	}
	if !strings.HasPrefix(w.Env["GUARD"], "github.com/magicdrive/hephaestus/cmd/tap-version-guard@") || w.Env["GOTOOLCHAIN"] != "local" {
		t.Errorf("env %v", w.Env)
	}
	if strings.Contains(raw, "-allow-downgrade") {
		t.Error("the example uses -allow-downgrade: a label alone would lower every downgrade")
	}
	// A skipped job reports success: the required check depends on no
	// job, and no other job is named version-guard.
	if g := w.Jobs["version-guard"]; g.Needs != nil || g.If != "github.event_name == 'pull_request_target' || github.event_name == 'merge_group'" {
		t.Errorf("version-guard: needs %v if %q", g.Needs, g.If)
	}
	for name, job := range w.Jobs {
		if name != "version-guard" && (job.Name == "version-guard" || strings.Contains(name, "version-guard") && job.Name != "") {
			t.Errorf("%s: named %q", name, job.Name)
		}
	}
	if v := w.Jobs["version-guard-verdict"]; !slices.Equal(toStrings(v.Needs), []string{"version-guard-pending", "version-guard"}) ||
		v.If != "${{ !cancelled() && github.event_name == 'pull_request_target' }}" {
		t.Errorf("version-guard-verdict: needs %v if %q", v.Needs, v.If)
	}
	if w.Jobs["version-guard-pending"].If != "github.event_name == 'pull_request_target'" {
		t.Errorf("version-guard-pending: if %q", w.Jobs["version-guard-pending"].If)
	}
	pending, verdict := w.step(t, "version-guard-pending", "Commit status"), w.step(t, "version-guard-verdict", "Commit status")
	if pending.Run != verdict.Run || pending.Env["RESULT"] != "pending" || verdict.Env["RESULT"] != "${{ needs.version-guard.result }}" {
		t.Errorf("the commit status steps differ: %q / %q", pending.Env["RESULT"], verdict.Env["RESULT"])
	}
	pinned := regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)
	writes := map[string]bool{"version-guard-pending": true, "version-guard-verdict": true, "after-merge-issue": true}
	for name, job := range w.Jobs {
		for _, s := range job.Steps {
			where := name + "/" + s.Name + s.Uses
			if s.Uses != "" {
				if !pinned.MatchString(s.Uses) || !regexp.MustCompile(regexp.QuoteMeta(s.Uses)+` # v\d`).MatchString(raw) {
					t.Errorf("%s: not pinned to a commit SHA with a # vX.Y.Z comment", where)
				}
				if writes[name] {
					t.Errorf("%s: a job that writes runs no action", where)
				}
			}
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				if s.With["persist-credentials"] != "false" {
					t.Errorf("%s: persist-credentials must be false", where)
				}
				// pull_request_target checks out the default branch, never
				// the pull request (its files are read as git objects).
				if _, ok := s.With["ref"]; ok {
					t.Errorf("%s: checks out %q", where, s.With["ref"])
				}
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
			if writes[name] && (strings.Contains(s.Run, "GUARD") || strings.Contains(s.Run, "git ") || strings.Contains(s.Run, "guard\"")) {
				t.Errorf("%s: a job that writes runs no guard and no git", where)
			}
			if name == "version-guard" && (strings.Contains(s.Run, "checkout") || strings.Contains(s.Run, "git switch") || strings.Contains(s.Run, "git worktree")) {
				t.Errorf("%s: the pull request is never checked out", where)
			}
		}
	}
	if j := w.Jobs["after-merge-issue"]; j.Needs != "after-merge" || j.If != "always() && needs.after-merge.outputs.found == '1'" {
		t.Errorf("after-merge-issue: needs %q if %q", j.Needs, j.If)
	}
}

func toStrings(v any) []string {
	l, _ := v.([]any)
	var s []string
	for _, x := range l {
		str, _ := x.(string)
		s = append(s, str)
	}
	return s
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
fail() { echo "HTTP 502" >&2; exit 1; }
if [ "$1 ${2:-}" = "pr list" ]; then
  [ -e "$STUB_DIR/prs.fail" ] && fail
  cat "$STUB_DIR/prs" 2>/dev/null || true
  exit 0
fi
[ "$1" = api ] || { echo "unexpected gh $*" >&2; exit 3; }
path="" method=GET
for a in "${@:2}"; do
  case "$a" in
  repos/*) path="$a" ;;
  POST) method=POST ;;
  esac
done
case "$method $path" in
"POST repos/"*/statuses/*)
  [ -e "$STUB_DIR/statuses.fail" ] && fail
  printf '%s\n' "$*" >> "$STUB_DIR/statuses"
  echo '{}' ;;
"GET repos/"*/commits/*/pulls)
  [ -e "$STUB_DIR/pulls.fail" ] && fail
  sha="${path#*/commits/}"; sha="${sha%/pulls}"
  if [ -e "$STUB_DIR/pulls-$sha.json" ]; then cat "$STUB_DIR/pulls-$sha.json"; else echo '[]'; fi ;;
"GET repos/"*/pulls/*)
  [ -e "$STUB_DIR/pull.fail" ] && fail
  cat "$STUB_DIR/pull.json" ;;
"GET repos/"*/issues/*/comments)
  [ -e "$STUB_DIR/comments.fail" ] && fail
  n="${path%/comments}"; n="${n##*/}"
  if [ -e "$STUB_DIR/comments-$n.json" ]; then cat "$STUB_DIR/comments-$n.json"; else echo '[]'; fi ;;
"GET repos/"*/collaborators/*/permission)
  [ -e "$STUB_DIR/permission.fail" ] && fail
  login="${path%/permission}"; login="${login##*/}"
  [ -e "$STUB_DIR/perm-$login.json" ] || { echo "HTTP 404: Not Found" >&2; exit 1; }
  cat "$STUB_DIR/perm-$login.json" ;;
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
// and returns its output and the outputs it set; it fails the test when
// the script fails.
func runStep(t *testing.T, s wfStep, dir, bin, stub string, env map[string]string) (string, map[string]string) {
	t.Helper()
	out, outputs, code := runStepCode(t, s, dir, bin, stub, env)
	if code != 0 {
		t.Fatalf("step %q: exit %d\n%s", s.Name, code, out)
	}
	return out, outputs
}

// runStepCode is runStep for a script that may fail. env may set
// RUNNER_TEMP and GITHUB_STEP_SUMMARY (steps of one job share them).
func runStepCode(t *testing.T, s wfStep, dir, bin, stub string, env map[string]string) (string, map[string]string, int) {
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
	// Later entries win: env may replace RUNNER_TEMP and the summary.
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
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("step %q: %v\n%s", s.Name, err, b)
		}
		code = ee.ExitCode()
	}
	outputs := map[string]string{}
	data, _ := os.ReadFile(out)
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			outputs[k] = v
		}
	}
	return string(b), outputs, code
}

func envKeys(t *testing.T, s wfStep, want ...string) {
	t.Helper()
	if got := slices.Sorted(maps.Keys(s.Env)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("step %q env %v, the test sets %v", s.Name, got, want)
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
	runSummary := func(t *testing.T, a *afterMerge, work, event, before string, stub, bin, summary string) result {
		t.Helper()
		env := map[string]string{
			"EVENT": event, "BEFORE": before, "AFTER": a.git("rev-parse", "main"), "BRANCH": "main",
			"GH_TOKEN": "t", "REPO": "o/tap", "GIT_TERMINAL_PROMPT": "0",
			"GUARD": w.Env["GUARD"], "GOTOOLCHAIN": "local",
		}
		if summary != "" {
			env["GITHUB_STEP_SUMMARY"] = summary
		}
		out, o := runStep(t, s, work, bin, stub, env)
		rep, err := base64.StdEncoding.DecodeString(o["report"])
		if err != nil {
			t.Fatalf("report output: %v", err)
		}
		return result{o["found"], o["errors"], string(rep), out}
	}
	run := func(t *testing.T, a *afterMerge, work, event, before string, stub, bin string) result {
		t.Helper()
		return runSummary(t, a, work, event, before, stub, bin, "")
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
	// pullOf is the answer of commits/{sha}/pulls for the merge of B.
	pullOf := func(merge, head, mergedAt string, labels ...string) string {
		ls := []map[string]string{}
		for _, l := range labels {
			ls = append(ls, map[string]string{"name": l})
		}
		b, _ := json.Marshal([]map[string]any{{"number": 2, "merge_commit_sha": merge, "merged_at": mergedAt,
			"head": map[string]string{"sha": head}, "labels": ls}})
		return string(b)
	}
	const mergedAt = "2026-10-27T02:00:00Z"
	for _, tc := range []struct {
		name       string
		labels     []string
		approver   string // "" no approval comment
		permission string
		created    string
		otherHead  bool
		fail       string // a stub that fails
		found      string
		want       string
	}{
		{"labelled and approved by the admin before the merge", []string{"formula-rollback"}, ownerLogin, "admin", t0, false, "", "0", "(authorized rollback: approved by magicdrive"},
		{"the label alone", []string{"formula-rollback"}, "", "", "", false, "", "1", "version goes down from 2.0.0 to 1.5.0"},
		{"approved, not labelled", nil, ownerLogin, "admin", t0, false, "", "1", "version goes down"},
		{"approved by a writer", []string{"formula-rollback"}, "w", "write", t0, false, "", "1", "w has write"},
		{"approved after the merge", []string{"formula-rollback"}, ownerLogin, "admin", "2026-10-27T03:00:00Z", false, "", "1", "not before 2026-10-27T02:00:00Z (the merge)"},
		{"approved for another head", []string{"formula-rollback"}, ownerLogin, "admin", t0, true, "", "1", "it approves head"},
		{"comments unreadable", []string{"formula-rollback"}, ownerLogin, "admin", t0, false, "comments.fail", "1", "::warning::cannot read the rollback approvals of #2"},
		{"permission unreadable", []string{"formula-rollback"}, ownerLogin, "admin", t0, false, "permission.fail", "1", "::warning::cannot read the rollback approvals of #2"},
		{"pull request unreadable", []string{"formula-rollback"}, ownerLogin, "admin", t0, false, "pulls.fail", "1", "::warning::cannot read the pull request"},
	} {
		t.Run("A then B pushed, B "+tc.name, func(t *testing.T) {
			t.Parallel()
			bin, stub := stubs(t)
			a := newAfterMerge(t)
			a.merge("pr-a")
			head := a.git("rev-parse", "pr-b")
			mergeB := a.merge("pr-b")
			write(t, stub, "pulls-"+mergeB+".json", pullOf(mergeB, head, mergedAt, tc.labels...))
			if tc.approver != "" {
				approved := head
				if tc.otherHead {
					approved = strings.Repeat("b", 40)
				}
				approve(t, stub, 2, fakeComment{9, tc.approver, approvalBody(approved, "tool.rb 2.0.0 -> 1.5.0"), tc.created, tc.created}, tc.permission)
			}
			if tc.fail != "" {
				write(t, stub, tc.fail, "")
			}
			summary := filepath.Join(t.TempDir(), "summary")
			got := runSummary(t, a, a.publish(nil), "push", a.oldBase, stub, bin, summary)
			if got.found != tc.found || got.errors != "0" || !strings.Contains(got.out, tc.want) {
				t.Fatalf("%+v", got)
			}
			sum, _ := os.ReadFile(summary)
			if audited := strings.Contains(string(sum), "| tool.rb | 2.0.0 → 1.5.0 | magicdrive |"); audited != (tc.found == "0") {
				t.Fatalf("audit trail %v:\n%s", audited, sum)
			}
		})
	}
	t.Run("A then B pushed, the approval of another commit does not count", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		a := newAfterMerge(t)
		mergeA := a.merge("pr-a")
		head := a.git("rev-parse", "pr-b")
		mergeB := a.merge("pr-b")
		write(t, stub, "pulls-"+mergeB+".json", pullOf(mergeA, head, mergedAt, "formula-rollback"))
		approve(t, stub, 2, fakeComment{9, ownerLogin, approvalBody(head, "tool.rb 2.0.0 -> 1.5.0"), t0, t0}, "admin")
		if got := run(t, a, a.publish(nil), "push", a.oldBase, stub, bin); got.found != "1" {
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
		if got := run(t, a, work, "workflow_dispatch", "", stub, bin); got.found != "1" {
			t.Fatalf("the label alone: %+v", got)
		}
		approve(t, stub, 2, fakeComment{9, ownerLogin, approvalBody(a.git("rev-parse", "pr-b"), "tool.rb 2.0.0 -> 1.5.0"), t0, t0}, "admin")
		if got := run(t, a, work, "workflow_dispatch", "", stub, bin); got.found != "0" || got.errors != "0" {
			t.Fatalf("labelled and approved: %+v", got)
		}
		write(t, stub, "prs", "2 "+a.git("rev-parse", "pr-b")+" false\n")
		if got := run(t, a, work, "workflow_dispatch", "", stub, bin); got.found != "1" {
			t.Fatalf("approved, label removed: %+v", got)
		}
		write(t, stub, "prs", "2 "+a.git("rev-parse", "pr-b")+" true\n")
		write(t, stub, "comments.fail", "")
		if got := run(t, a, work, "workflow_dispatch", "", stub, bin); got.found != "1" || !strings.Contains(got.out, "cannot read the rollback approvals of #2") {
			t.Fatalf("approvals unreadable: %+v", got)
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

// approve stubs the approval comment c on pull request n and the
// permission of its author.
func approve(t *testing.T, stub string, n int, c fakeComment, permission string) {
	t.Helper()
	writeJSON(t, filepath.Join(stub, "comments-"+strconv.Itoa(n)+".json"), []map[string]any{c.json()})
	writeJSON(t, filepath.Join(stub, "perm-"+c.login+".json"), perm(c.login, permission, permission))
}
