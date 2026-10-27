package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prTarget is a tap whose main has tool.rb at 2.0.0 and other.rb at
// 3.0.0, with pull request 7 (branch pr) opened on it, published to a
// local origin (refs/heads/main, refs/pull/7/head); work is the clone the
// checkout leaves: the default branch, never the pull request.
type prTarget struct {
	*tapRepo
	origin, work, head string
}

func newPRTarget(t *testing.T, change map[string]string) *prTarget {
	t.Helper()
	r := newTap(t)
	r.commit(map[string]string{"tool.rb": brew("2.0.0"), "other.rb": formula("3.0.0"), "README.md": "tap"})
	r.git("checkout", "-q", "-b", "pr", "main")
	p := &prTarget{tapRepo: r, origin: filepath.Join(t.TempDir(), "origin.git"), head: r.commit(change)}
	r.git("checkout", "-q", "main")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", p.origin).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p.push()
	p.work = filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "clone", "-q", p.origin, p.work).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return p
}

func (p *prTarget) push() {
	p.git("push", "-q", "--force", p.origin, "main:refs/heads/main", "pr:refs/pull/7/head")
}

// pushPR adds a commit to the pull request (a synchronize event).
func (p *prTarget) pushPR(change map[string]string) {
	p.git("checkout", "-q", "pr")
	p.head = p.commit(change)
	p.git("checkout", "-q", "main")
	p.push()
}

type prVerdict struct {
	green   bool
	failed  string // the step that failed
	out     string
	summary string
	calls   string
}

// pullJSON is the answer of GET repos/o/tap/pulls/7.
func pullJSON(t *testing.T, stub, head string, labels ...string) {
	t.Helper()
	ls := []map[string]string{}
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	writeJSON(t, filepath.Join(stub, "pull.json"), map[string]any{"number": 7, "head": map[string]string{"sha": head}, "labels": ls})
}

// runVersionGuard runs the steps of the version-guard job of a
// pull_request_target event for p's head (eventHead when set) on one
// runner, as Actions would: the first failing step fails the check.
func runVersionGuard(t *testing.T, w workflow, p *prTarget, bin, stub, eventHead, baseRef string) (v prVerdict) {
	t.Helper()
	if eventHead == "" {
		eventHead = p.head
	}
	if baseRef == "" {
		baseRef = "main"
	}
	temp := t.TempDir()
	summary := filepath.Join(temp, "step-summary.md")
	common := map[string]string{"RUNNER_TEMP": temp, "GITHUB_STEP_SUMMARY": summary, "GUARD": w.Env["GUARD"], "GOTOOLCHAIN": "local"}
	with := func(env map[string]string) map[string]string {
		for k, v := range common {
			env[k] = v
		}
		return env
	}
	defer func() {
		s, _ := os.ReadFile(summary)
		c, _ := os.ReadFile(filepath.Join(stub, "calls"))
		v.summary, v.calls = string(s), string(c)
	}()
	approval := w.step(t, "version-guard", "Rollback approval")
	envKeys(t, approval, "GH_TOKEN", "REPO", "PR", "HEAD_SHA")
	out, outputs, code := runStepCode(t, approval, p.work, bin, stub, with(map[string]string{"GH_TOKEN": "t", "REPO": "o/tap", "PR": "7", "HEAD_SHA": eventHead}))
	v.out += out
	if code != 0 {
		v.failed = approval.Name
		return v
	}
	fetch := w.step(t, "version-guard", "Fetch the pull request")
	envKeys(t, fetch, "GH_TOKEN", "PR", "HEAD_SHA", "BASE_REF", "GIT_TERMINAL_PROMPT")
	out, _, code = runStepCode(t, fetch, p.work, bin, stub, with(map[string]string{"GH_TOKEN": "t", "PR": "7", "HEAD_SHA": eventHead, "BASE_REF": baseRef, "GIT_TERMINAL_PROMPT": "0"}))
	v.out += out
	if code != 0 {
		v.failed = fetch.Name
		return v
	}
	refuse := w.step(t, "version-guard", "Refuse formula downgrades")
	envKeys(t, refuse, "EVENT", "HEAD_SHA", "ROLLBACK", "QUEUE_BASE")
	out, _, code = runStepCode(t, refuse, p.work, bin, stub, with(map[string]string{"EVENT": "pull_request_target", "HEAD_SHA": eventHead, "ROLLBACK": outputs["rollback"], "QUEUE_BASE": ""}))
	v.out += out
	if code != 0 {
		v.failed = refuse.Name
		return v
	}
	v.green = true
	return v
}

// TestExampleWorkflow_PullRequestTarget: the required check, end to end
// on local repositories with a fake GitHub API. A rollback passes only
// with the label and an admin's approval of exactly this head, formula
// and versions; anything that cannot be read fails.
func TestExampleWorkflow_PullRequestTarget(t *testing.T) {
	w, _ := readExample(t)
	down := map[string]string{"tool.rb": brew("1.5.0")}
	const label = "formula-rollback"
	ok := "tool.rb 2.0.0 -> 1.5.0"
	type setup struct {
		stub string
		p    *prTarget
	}
	approveBy := func(t *testing.T, s setup, login, permission, role string, formulae ...string) {
		t.Helper()
		if len(formulae) == 0 {
			formulae = []string{ok}
		}
		writeJSON(t, filepath.Join(s.stub, "comments-7.json"), []map[string]any{fakeComment{11, login, approvalBody(s.p.head, formulae...), "", ""}.json()})
		writeJSON(t, filepath.Join(s.stub, "perm-"+login+".json"), perm(login, permission, role))
	}
	for _, tc := range []struct {
		name    string
		change  map[string]string
		prepare func(t *testing.T, s setup)
		green   bool
		want    []string // in the output
		failed  string   // the step that fails
		audit   bool
		noCalls []string // gh calls that must not happen
	}{
		{name: "upgrade", change: map[string]string{"tool.rb": brew("2.1.0")},
			prepare: func(t *testing.T, s setup) { pullJSON(t, s.stub, s.p.head) },
			green:   true, want: []string{"ok tool.rb: 2.0.0 -> 2.1.0"}, noCalls: []string{"/comments", "/permission"}},
		{name: "downgrade, no label, no approval", change: down,
			prepare: func(t *testing.T, s setup) { pullJSON(t, s.stub, s.p.head) },
			want:    []string{"::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0"}, failed: "Refuse formula downgrades", noCalls: []string{"/comments"}},
		{name: "downgrade, the label alone", change: down,
			prepare: func(t *testing.T, s setup) { pullJSON(t, s.stub, s.p.head, label) },
			want:    []string{"::error file=tool.rb::tool.rb: version goes down"}, failed: "Refuse formula downgrades"},
		{name: "approved by a writer", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, "w", "write", "write")
			},
			want: []string{"w has write (role write)", "::error file=tool.rb::"}, failed: "Refuse formula downgrades"},
		{name: "approved by a maintainer", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, "m", "write", "maintain")
			},
			want: []string{"(role maintain)"}, failed: "Refuse formula downgrades"},
		{name: "approved by a triager", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, "tr", "read", "triage")
			},
			want: []string{"(role triage)"}, failed: "Refuse formula downgrades"},
		{name: "approved by a reader", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, "r", "read", "read")
			},
			want: []string{"r has read"}, failed: "Refuse formula downgrades"},
		{name: "approved by the fork's author, author_association OWNER", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, "forker", "read", "read")
			},
			want: []string{"forker has read"}, failed: "Refuse formula downgrades"},
		{name: "approved by the admin", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin")
			},
			green: true, audit: true,
			want: []string{"::warning file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0 (authorized rollback: approved by magicdrive in https://github.com/o/tap/pull/7#issuecomment-11",
				"::notice title=formula rollback authorized::tool.rb 2.0.0 -> 1.5.0 approved by magicdrive"}},
		{name: "approved for another formula", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin", "other.rb 3.0.0 -> 2.0.0")
			},
			want: []string{"::error file=tool.rb::tool.rb: version goes down", "::error file=other.rb::other.rb: a rollback from 3.0.0 to 2.0.0 is authorized"}, failed: "Refuse formula downgrades"},
		{name: "approved for another version", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin", "tool.rb 2.0.0 -> 1.4.0")
			},
			want: []string{"::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0"}, failed: "Refuse formula downgrades"},
		{name: "a second formula lowered beside the approved one", change: map[string]string{"tool.rb": brew("1.5.0"), "other.rb": formula("2.0.0")},
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin")
			},
			want: []string{"::warning file=tool.rb::", "::error file=other.rb::other.rb: version goes down from 3.0.0 to 2.0.0"}, failed: "Refuse formula downgrades"},
		{name: "head pushed after the approval", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin")
				s.p.pushPR(map[string]string{"tool.rb": strings.Replace(brew("1.5.0"), `desc "tool"`, `desc "x"`, 1)})
				pullJSON(t, s.stub, s.p.head, label)
			},
			want: []string{"it approves head"}, failed: "Refuse formula downgrades"},
		{name: "the run of an older head", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, strings.Repeat("c", 40), label)
				approveBy(t, s, ownerLogin, "admin", "admin")
			},
			want: []string{"::error::the pull request head moved to cccc"}, failed: "Rollback approval"},
		{name: "label removed", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head)
				approveBy(t, s, ownerLogin, "admin", "admin")
			},
			want: []string{"No formula-rollback label", "::error file=tool.rb::"}, failed: "Refuse formula downgrades", noCalls: []string{"/comments", "/permission"}},
		{name: "edited approval", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				writeJSON(t, filepath.Join(s.stub, "comments-7.json"), []map[string]any{fakeComment{11, ownerLogin, approvalBody(s.p.head, ok), t0, "2026-10-27T01:30:00Z"}.json()})
				writeJSON(t, filepath.Join(s.stub, "perm-"+ownerLogin+".json"), perm(ownerLogin, "admin", "admin"))
			},
			want: []string{"the comment was edited"}, failed: "Refuse formula downgrades"},
		{name: "malformed approval", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				writeJSON(t, filepath.Join(s.stub, "comments-7.json"), []map[string]any{fakeComment{11, ownerLogin, "/formula-rollback\nformula: " + ok + "\nreason: x\n", "", ""}.json()})
				writeJSON(t, filepath.Join(s.stub, "perm-"+ownerLogin+".json"), perm(ownerLogin, "admin", "admin"))
			},
			want: []string{"malformed: want exactly one head: line"}, failed: "Refuse formula downgrades"},
		{name: "pull request unreadable", change: down,
			prepare: func(t *testing.T, s setup) { os.WriteFile(filepath.Join(s.stub, "pull.fail"), nil, 0o644) },
			want:    []string{"HTTP 502"}, failed: "Rollback approval"},
		{name: "comments unreadable", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin")
				os.WriteFile(filepath.Join(s.stub, "comments.fail"), nil, 0o644)
			},
			want: []string{"HTTP 502"}, failed: "Rollback approval"},
		{name: "permission unreadable", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin")
				os.WriteFile(filepath.Join(s.stub, "permission.fail"), nil, 0o644)
			},
			want: []string{"HTTP 502"}, failed: "Rollback approval"},
		{name: "permission of an unknown user (404)", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				writeJSON(t, filepath.Join(s.stub, "comments-7.json"), []map[string]any{fakeComment{11, "ghost", approvalBody(s.p.head, ok), "", ""}.json()})
			},
			want: []string{"HTTP 404"}, failed: "Rollback approval"},
		{name: "a bot's approval is ignored, not looked up", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				writeJSON(t, filepath.Join(s.stub, "comments-7.json"), []map[string]any{fakeComment{11, "renovate[bot]", approvalBody(s.p.head, ok), "", ""}.json()})
			},
			want: []string{"its author is not a user account"}, failed: "Refuse formula downgrades", noCalls: []string{"/permission"}},
		{name: "someone else's comment does not block the admin's approval", change: down,
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				writeJSON(t, filepath.Join(s.stub, "comments-7.json"), []map[string]any{
					fakeComment{10, "forker", approvalBody(s.p.head, "tool.rb 2.0.0 -> 1.0.0"), "", ""}.json(),
					fakeComment{11, ownerLogin, approvalBody(s.p.head, ok), "", ""}.json(),
				})
				writeJSON(t, filepath.Join(s.stub, "perm-forker.json"), perm("forker", "read", "read"))
				writeJSON(t, filepath.Join(s.stub, "perm-"+ownerLogin+".json"), perm(ownerLogin, "admin", "admin"))
			},
			green: true, audit: true, want: []string{"forker has read"}},
		{name: "removal, approved by the admin", change: map[string]string{"tool.rb": ""},
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, ownerLogin, "admin", "admin")
			},
			want: []string{"tool.rb: formula removed (was 2.0.0)", "is authorized"}, failed: "Refuse formula downgrades"},
		// A fork pull request that rewrites this workflow and brings
		// executables: the steps that run are the default branch's, the
		// work tree stays the default branch, nothing of the head runs.
		{name: "fork escalation: the head rewrites the workflow", change: map[string]string{
			"tool.rb": brew("1.5.0"),
			".github/workflows/formula-version-guard.yml": "on: pull_request\njobs:\n  version-guard:\n    runs-on: ubuntu-latest\n    steps:\n      - run: exit 0\n",
			"gh":         "#!/bin/sh\ntouch \"$STUB_DIR/pwned\"\n",
			"go":         "#!/bin/sh\ntouch \"$STUB_DIR/pwned\"\n",
			"Makefile":   "all:\n\ttouch \"$$STUB_DIR/pwned\"\n",
			"evil.txt":   "x",
			".gitconfig": "[core]\n\tfsmonitor = touch pwned\n",
		},
			prepare: func(t *testing.T, s setup) {
				pullJSON(t, s.stub, s.p.head, label)
				approveBy(t, s, "forker", "read", "read")
			},
			want: []string{"::error file=tool.rb::tool.rb: version goes down", "forker has read"}, failed: "Refuse formula downgrades"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, stub := stubs(t)
			p := newPRTarget(t, tc.change)
			tc.prepare(t, setup{stub, p})
			v := runVersionGuard(t, w, p, bin, stub, "", "")
			if v.green != tc.green || v.failed != tc.failed {
				t.Fatalf("green %v failed %q, want %v %q\n%s", v.green, v.failed, tc.green, tc.failed, v.out)
			}
			for _, s := range tc.want {
				if !strings.Contains(v.out, s) {
					t.Errorf("output lacks %q\n%s", s, v.out)
				}
			}
			for _, c := range tc.noCalls {
				if strings.Contains(v.calls, c) {
					t.Errorf("gh called %s:\n%s", c, v.calls)
				}
			}
			audited := strings.Contains(v.summary, "| tool.rb | 2.0.0 → 1.5.0 | magicdrive | https://github.com/o/tap/pull/7#issuecomment-11 | `"+p.head+"` | 2.0.0 corrupts the cache (#12) |")
			if audited != tc.audit {
				t.Errorf("audit trail %v, want %v:\n%s", audited, tc.audit, v.summary)
			}
			if _, err := os.Stat(filepath.Join(stub, "pwned")); err == nil {
				t.Fatal("code of the pull request ran")
			}
			for _, f := range []string{"evil.txt", "gh", ".github/workflows/formula-version-guard.yml"} {
				if _, err := os.Stat(filepath.Join(p.work, f)); err == nil {
					t.Fatalf("%s of the pull request is in the work tree", f)
				}
			}
			if got := strings.TrimSpace(gitOut(t, p.work, "rev-parse", "HEAD")); got != p.git("rev-parse", "main") {
				t.Fatalf("the work tree moved to %s", got)
			}
		})
	}
	t.Run("crafted base branch", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		p := newPRTarget(t, map[string]string{"tool.rb": brew("2.1.0")})
		pullJSON(t, stub, p.head)
		for _, ref := range []string{"main $(touch pwned)", "-x", "main:refs/heads/x", "../main"} {
			if v := runVersionGuard(t, w, p, bin, stub, "", ref); v.green || v.failed != "Fetch the pull request" {
				t.Fatalf("%q: %+v", ref, v)
			}
		}
		if _, err := os.Stat(filepath.Join(p.work, "pwned")); err == nil {
			t.Fatal("the base ref ran a command")
		}
	})
	t.Run("crafted event values", func(t *testing.T) {
		t.Parallel()
		bin, stub := stubs(t)
		p := newPRTarget(t, down)
		pullJSON(t, stub, p.head, label)
		for _, head := range []string{"$(touch pwned)", strings.ToUpper(p.head), p.head + "\n"} {
			if v := runVersionGuard(t, w, p, bin, stub, head, ""); v.green || v.failed != "Rollback approval" {
				t.Fatalf("%q: %+v", head, v)
			}
		}
		if _, err := os.Stat(filepath.Join(p.work, "pwned")); err == nil {
			t.Fatal("an event value ran a command")
		}
	})
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestExampleWorkflow_MergeQueue: a queue entry checks every step and
// never takes an approval.
func TestExampleWorkflow_MergeQueue(t *testing.T) {
	w, _ := readExample(t)
	s := w.step(t, "version-guard", "Refuse formula downgrades")
	bin, stub := stubs(t)
	p := newPRTarget(t, map[string]string{"tool.rb": brew("1.5.0")})
	base := p.git("rev-parse", "main")
	p.git("merge", "-q", "--no-ff", "--no-edit", "pr")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "approval"), 0o755)
	os.WriteFile(filepath.Join(dir, "approval", "comments.json"), []byte("[]"), 0o644)
	os.WriteFile(filepath.Join(dir, "approval", "permissions.json"), nil, 0o644)
	out, _, code := runStepCode(t, s, p.dir, bin, stub, map[string]string{
		"EVENT": "merge_group", "HEAD_SHA": "", "ROLLBACK": "true", "QUEUE_BASE": base,
		"RUNNER_TEMP": dir, "GUARD": w.Env["GUARD"], "GOTOOLCHAIN": "local",
	})
	if code != 1 || !strings.Contains(out, "version goes down from 2.0.0 to 1.5.0") || strings.Contains(out, "approval") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// TestExampleWorkflow_CommitStatus: the verdict reaches the head as the
// commit status version-guard; anything but success is a failure.
func TestExampleWorkflow_CommitStatus(t *testing.T) {
	w, _ := readExample(t)
	s := w.step(t, "version-guard-verdict", "Commit status")
	envKeys(t, s, "RESULT", "GH_TOKEN", "REPO", "HEAD_SHA", "RUN_URL")
	head := strings.Repeat("ab", 20)
	for _, tc := range []struct{ result, state string }{
		{"pending", "pending"}, {"success", "success"}, {"failure", "failure"},
		{"cancelled", "failure"}, {"skipped", "failure"}, {"", "failure"}, {"Success", "failure"},
	} {
		bin, stub := stubs(t)
		out, _, code := runStepCode(t, s, t.TempDir(), bin, stub, map[string]string{
			"RESULT": tc.result, "GH_TOKEN": "t", "REPO": "o/tap", "HEAD_SHA": head, "RUN_URL": "https://github.com/o/tap/actions/runs/1",
		})
		calls, _ := os.ReadFile(filepath.Join(stub, "statuses"))
		want := "api -X POST repos/o/tap/statuses/" + head + " -f state=" + tc.state + " -f context=version-guard -f description="
		if code != 0 || !strings.HasPrefix(string(calls), want) || !strings.Contains(string(calls), "-f target_url=https://github.com/o/tap/actions/runs/1") {
			t.Errorf("%q: exit %d, calls %q\n%s", tc.result, code, calls, out)
		}
	}
	for _, bad := range []string{"", "$(touch pwned)", strings.ToUpper(head), head + "0"} {
		bin, stub := stubs(t)
		dir := t.TempDir()
		_, _, code := runStepCode(t, s, dir, bin, stub, map[string]string{"RESULT": "success", "GH_TOKEN": "t", "REPO": "o/tap", "HEAD_SHA": bad, "RUN_URL": "u"})
		if _, err := os.Stat(filepath.Join(stub, "statuses")); code == 0 || err == nil {
			t.Errorf("head %q: exit %d, posted %v", bad, code, err == nil)
		}
		if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
			t.Fatal("the head ran a command")
		}
	}
	bin, stub := stubs(t)
	os.WriteFile(filepath.Join(stub, "statuses.fail"), nil, 0o644)
	if _, _, code := runStepCode(t, s, t.TempDir(), bin, stub, map[string]string{"RESULT": "success", "GH_TOKEN": "t", "REPO": "o/tap", "HEAD_SHA": head, "RUN_URL": "u"}); code == 0 {
		t.Error("a failed post passes")
	}
}
