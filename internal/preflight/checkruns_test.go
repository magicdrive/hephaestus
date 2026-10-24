package preflight

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
)

const actionsApp = 15368 // github-actions

func run(id int64, name string, app int64, status, conclusion string, started string) github.CheckRun {
	r := github.CheckRun{ID: id, Name: name, Status: status, Conclusion: conclusion}
	if app != 0 {
		r.App = &struct {
			ID   int64  `json:"id"`
			Slug string `json:"slug"`
		}{app, map[int64]string{actionsApp: "github-actions", 99: "other-ci"}[app]}
	}
	if started != "" {
		ts, err := time.Parse(time.RFC3339, started)
		if err != nil {
			panic(err)
		}
		r.StartedAt = &ts
	}
	return r
}

func done(id int64, name, conclusion, started string) github.CheckRun {
	return run(id, name, actionsApp, "completed", conclusion, started)
}

// ciWith runs github.ci with the given runs (and the default rulesets,
// which require ci-gate from any source).
func ciWith(t *testing.T, runs []github.CheckRun, setup func(f *fixture)) Result {
	t.Helper()
	f := newFixture(t, "v1.2.0")
	f.gh.runs = runs
	if setup != nil {
		setup(f)
	}
	return result(t, f.run(t, false), "github.ci")
}

func TestCheckRunSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runs   []github.CheckRun
		setup  func(f *fixture)
		status Status
		msg    string
	}{
		{"two successes", []github.CheckRun{done(1, "ci-gate", "success", "2026-10-24T01:00:00Z"), done(2, "ci-gate", "success", "2026-10-24T02:00:00Z")}, nil, Pass, "1 check succeeded"},
		{"old success, new failure", []github.CheckRun{done(1, "ci-gate", "success", "2026-10-24T01:00:00Z"), done(2, "ci-gate", "failure", "2026-10-24T02:00:00Z")}, nil, Fail, "ci-gate [github-actions] (failure)"},
		{"new failure listed first", []github.CheckRun{done(2, "ci-gate", "failure", "2026-10-24T02:00:00Z"), done(1, "ci-gate", "success", "2026-10-24T01:00:00Z")}, nil, Fail, "(failure)"},
		{"old failure, new success (a re-run)", []github.CheckRun{done(1, "ci-gate", "failure", "2026-10-24T01:00:00Z"), done(2, "ci-gate", "success", "2026-10-24T02:00:00Z")}, nil, Pass, "succeeded"},
		{"new success listed first", []github.CheckRun{done(2, "ci-gate", "success", "2026-10-24T02:00:00Z"), done(1, "ci-gate", "failure", "2026-10-24T01:00:00Z")}, nil, Pass, "succeeded"},
		{"newer run still pending", []github.CheckRun{done(1, "ci-gate", "success", "2026-10-24T01:00:00Z"), run(2, "ci-gate", actionsApp, "in_progress", "", "2026-10-24T02:00:00Z")}, nil, Fail, "still running"},
		{"newer run queued without started_at", []github.CheckRun{done(1, "ci-gate", "success", "2026-10-24T01:00:00Z"), run(2, "ci-gate", actionsApp, "queued", "", "")}, nil, Fail, "still running"},
		{"same name from another app fails", []github.CheckRun{done(1, "ci-gate", "success", "2026-10-24T01:00:00Z"), run(2, "ci-gate", 99, "completed", "failure", "2026-10-24T00:00:00Z")}, nil, Fail, "ci-gate [other-ci] (failure)"},
		{"required app not reported", []github.CheckRun{run(5, "ci-gate", 99, "completed", "success", "2026-10-24T01:00:00Z")}, func(f *fixture) {
			f.gh.rulesets[0].Rules[2].Parameters.RequiredStatusChecks[0].IntegrationID = actionsApp
		}, Fail, "ci-gate (app 15368): reported only by ci-gate [other-ci]"},
		{"required app reported", []github.CheckRun{done(5, "ci-gate", "success", "2026-10-24T01:00:00Z"), run(6, "ci-gate", 99, "completed", "success", "")}, func(f *fixture) {
			f.gh.rulesets[0].Rules[2].Parameters.RequiredStatusChecks[0].IntegrationID = actionsApp
		}, Pass, "2 checks succeeded"},
		{"same started_at, IDs decide", []github.CheckRun{done(2, "ci-gate", "failure", "2026-10-24T01:00:00Z"), done(3, "ci-gate", "success", "2026-10-24T01:00:00Z")}, nil, Pass, "succeeded"},
		{"no IDs, started_at decides", []github.CheckRun{done(0, "ci-gate", "success", "2026-10-24T02:00:00Z"), done(0, "ci-gate", "failure", "2026-10-24T01:00:00Z")}, nil, Pass, "succeeded"},
		{"no IDs, same time, different outcomes", []github.CheckRun{done(0, "ci-gate", "success", "2026-10-24T01:00:00Z"), done(0, "ci-gate", "failure", "2026-10-24T01:00:00Z")}, nil, Unknown, "cannot tell which run is the latest"},
		{"no IDs, no times, same outcome", []github.CheckRun{done(0, "ci-gate", "success", ""), done(0, "ci-gate", "success", "")}, nil, Pass, "succeeded"},
		{"no IDs, no times, different outcomes", []github.CheckRun{done(0, "ci-gate", "failure", ""), done(0, "ci-gate", "success", "")}, nil, Unknown, "ambiguous"},
		{"completed without conclusion", []github.CheckRun{run(1, "ci-gate", actionsApp, "completed", "", "")}, nil, Fail, "(unknown)"},
		{"check runs unreadable", nil, func(f *fixture) { f.gh.errs["ListCheckRuns"] = apiErr(500, "boom") }, Unknown, "500"},
		{"commit status unreadable", []github.CheckRun{done(1, "ci-gate", "success", "")}, func(f *fixture) { f.gh.errs["GetCombinedStatus"] = apiErr(403, "no") }, Unknown, "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ciWith(t, tc.runs, tc.setup)
			if r.Status != tc.status || !strings.Contains(r.Message, tc.msg) {
				t.Errorf("github.ci = %s %q, want %s containing %q\nevidence %v", r.Status, r.Message, tc.status, tc.msg, r.Evidence)
			}
		})
	}
}

// TestCheckRunSelectionIsOrderIndependent: every permutation of the same
// runs gives the same groups.
func TestCheckRunSelectionIsOrderIndependent(t *testing.T) {
	runs := []github.CheckRun{
		done(10, "ci-gate", "failure", "2026-10-24T01:00:00Z"),
		done(30, "ci-gate", "success", "2026-10-24T03:00:00Z"),
		run(20, "ci-gate", actionsApp, "in_progress", "", "2026-10-24T02:00:00Z"),
		done(11, "test", "success", "2026-10-24T01:00:00Z"),
		run(12, "test", 99, "completed", "failure", "2026-10-24T01:00:00Z"),
		done(0, "lint", "success", ""),
		done(0, "lint", "success", ""),
	}
	want := describeAll(latestCheckRuns(runs))
	var permute func(k int)
	n := 0
	permute = func(k int) {
		if k == len(runs) {
			n++
			if got := describeAll(latestCheckRuns(slices.Clone(runs))); got != want {
				t.Fatalf("order %v gave\n%s\nwant\n%s", ids(runs), got, want)
			}
			return
		}
		for i := k; i < len(runs); i++ {
			runs[k], runs[i] = runs[i], runs[k]
			permute(k + 1)
			runs[k], runs[i] = runs[i], runs[k]
		}
	}
	permute(0)
	if n != 5040 {
		t.Fatalf("%d permutations", n)
	}
	for _, line := range []string{"ci-gate [github-actions]: success (run 30, 2 older superseded)", "test [other-ci]: failure (run 12)", "lint [github-actions]: success (run 0, 1 older superseded)"} {
		if !strings.Contains(want, line) {
			t.Errorf("groups lack %q:\n%s", line, want)
		}
	}
}

func describeAll(gs []checkGroup) string {
	var out []string
	for _, g := range gs {
		out = append(out, g.describe())
	}
	return strings.Join(out, "\n")
}

func ids(rs []github.CheckRun) []int64 {
	var out []int64
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}
