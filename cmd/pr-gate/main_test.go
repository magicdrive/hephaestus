package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
	fake "github.com/magicdrive/hephaestus/internal/prgate/prgatetest"
)

func baseArgs(extra ...string) []string {
	return append([]string{"-repo", fake.Owner + "/" + fake.Repo, "-pr", "7", "-head", fake.Head, "-base-sha", fake.Base, "-mergeable-wait", "1ms"}, extra...)
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func runCLI(t *testing.T, s *fake.Server, args []string, getenv func(string) string) (int, string, string) {
	t.Helper()
	// Lock files go to a private temporary directory.
	setTempDir(t, t.TempDir())
	var out, errb bytes.Buffer
	opts := func(o *github.Options) {
		if s != nil {
			o.APIURL, o.UploadURL, o.HTTPClient = s.URL, s.URL, s.Client()
		}
		o.Sleep = func(time.Duration) {}
	}
	code := run(context.Background(), args, getenv, &out, &errb, opts)
	return code, out.String(), errb.String()
}

// setTempDir points os.TempDir at dir on every platform.
func setTempDir(t *testing.T, dir string) {
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
}

var token = env(map[string]string{"GH_TOKEN": "t"})

func TestDryRunIsTheDefaultAndNeverWrites(t *testing.T) {
	s := fake.New(t)
	code, out, errOut := runCLI(t, s, baseArgs(), token)
	if code != exitPass {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "GATE PASSED (dry run") {
		t.Errorf("output:\n%s", out)
	}
	if s.MergeCalls != 0 || len(s.Writes) != 0 {
		t.Fatalf("dry run wrote: %v", s.Writes)
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fake.Server)
		want   int
	}{
		{"draft fails", func(s *fake.Server) { s.Pull.Draft = true }, exitFail},
		{"api error is undecided", func(s *fake.Server) { s.Errors["check-runs"] = 500 }, exitUndecided},
		{"failure wins over an api error", func(s *fake.Server) { s.Pull.Draft = true; s.Errors["check-runs"] = 500 }, exitFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fake.New(t)
			tt.mutate(s)
			if code, out, _ := runCLI(t, s, baseArgs(), token); code != tt.want {
				t.Fatalf("exit %d, want %d\n%s", code, tt.want, out)
			}
			if len(s.Writes) != 0 {
				t.Fatalf("wrote: %v", s.Writes)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		getenv func(string) string
	}{
		{"no token", baseArgs(), env(nil)},
		{"short head", []string{"-repo", "o/r", "-pr", "7", "-head", fake.Head[:7], "-base-sha", fake.Base}, token},
		{"no base sha", []string{"-repo", "o/r", "-pr", "7", "-head", fake.Head}, token},
		{"no pr", []string{"-repo", "o/r", "-head", fake.Head, "-base-sha", fake.Base}, token},
		{"bad repo", []string{"-repo", "r", "-pr", "7", "-head", fake.Head, "-base-sha", fake.Base}, token},
		{"rebase", baseArgs("-merge", "-method", "rebase"), token},
		{"stray argument", baseArgs("merge"), token},
		{"unknown flag", baseArgs("-force"), token},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fake.New(t)
			if code, _, errOut := runCLI(t, s, tt.args, tt.getenv); code != exitUsage {
				t.Fatalf("exit %d, want %d\n%s", code, exitUsage, errOut)
			}
			if s.PullReads != 0 || len(s.Writes) != 0 {
				t.Fatal("API called on a usage error")
			}
		})
	}
}

func TestGITHUBTOKENIsAccepted(t *testing.T) {
	s := fake.New(t)
	if code, out, _ := runCLI(t, s, baseArgs(), env(map[string]string{"GITHUB_TOKEN": "t"})); code != exitPass {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestMergeFlag(t *testing.T) {
	s := fake.New(t)
	code, out, errOut := runCLI(t, s, baseArgs("-merge"), token)
	if code != exitPass || !strings.Contains(out, "MERGED o/r#7 as "+fake.Merge) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if s.MergeCalls != 1 || s.MergeBodies[0]["sha"] != fake.Head {
		t.Fatalf("merge requests %v", s.MergeBodies)
	}
}

func TestMergeFlag_GateFailureSendsNothing(t *testing.T) {
	s := fake.New(t)
	s.Pull.Draft = true
	if code, _, _ := runCLI(t, s, baseArgs("-merge"), token); code != exitFail {
		t.Fatalf("exit %d", code)
	}
	if len(s.Writes) != 0 {
		t.Fatalf("wrote: %v", s.Writes)
	}
}

func TestMergeFlag_Alarms(t *testing.T) {
	s := fake.New(t)
	s.MergeParents = []string{strings.Repeat("8", 40), fake.Head}
	code, out, _ := runCLI(t, s, baseArgs("-merge"), token)
	if code != exitAlarm || !strings.Contains(out, "MERGED BUT NOT VERIFIED") {
		t.Fatalf("exit %d\n%s", code, out)
	}

	s = fake.New(t)
	s.MergeStatus = 502
	s.OnPullRead = func(s *fake.Server, n int) {
		if n == 3 {
			s.Errors["pull"] = 502
		}
	}
	code, out, _ = runCLI(t, s, baseArgs("-merge"), token)
	if code != exitAlarm || !strings.Contains(out, "MERGE RESULT UNKNOWN") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestMergeFlag_Refused(t *testing.T) {
	s := fake.New(t)
	s.MergeStatus = 409
	code, out, _ := runCLI(t, s, baseArgs("-merge"), token)
	if code != exitFail || !strings.Contains(out, "NOT MERGED") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestMergeFlag_Lock(t *testing.T) {
	s := fake.New(t)
	setTempDir(t, t.TempDir())
	held := filepath.Join(os.TempDir(), "pr-gate-o-r-main.lock")
	if err := os.WriteFile(held, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run(context.Background(), baseArgs("-merge"), token, &out, &errb, func(o *github.Options) {
		o.APIURL, o.UploadURL, o.HTTPClient = s.URL, s.URL, s.Client()
	})
	if code != exitUndecided || !strings.Contains(errb.String(), "another pr-gate") {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if s.PullReads != 0 || len(s.Writes) != 0 {
		t.Fatal("API called while locked")
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatal("someone else's lock was removed")
	}
}

func TestJSONReport(t *testing.T) {
	s := fake.New(t)
	s.Pull.Draft = true
	code, out, _ := runCLI(t, s, baseArgs("-json"), token)
	if code != exitFail {
		t.Fatalf("exit %d", code)
	}
	var rep struct {
		Passed   bool
		Findings []struct{ ID, Status string }
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	found := false
	for _, f := range rep.Findings {
		if f.ID == "pr.draft" && f.Status == "FAIL" {
			found = true
		}
	}
	if rep.Passed || !found {
		t.Fatalf("report %+v", rep)
	}
}
