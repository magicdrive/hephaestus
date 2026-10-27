package prgate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/prgate"
	fake "github.com/magicdrive/hephaestus/internal/prgate/prgatetest"
)

func newGate(t *testing.T, s *fake.Server) *prgate.Gate {
	t.Helper()
	gh, err := github.New(github.Options{Token: "t", APIURL: s.URL, UploadURL: s.URL, HTTPClient: s.Client(), Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	return &prgate.Gate{GH: gh}
}

func config() prgate.Config {
	return prgate.Config{
		Owner: fake.Owner, Repo: fake.Repo, Number: fake.Number,
		HeadSHA: fake.Head, BaseRef: fake.BaseRef, BaseSHA: fake.Base,
		CIGate: prgate.DefaultCIGate, CIGateApp: prgate.GitHubActionsAppID,
		MergeableReads: 3, MergeableWait: time.Second,
	}
}

func evaluate(t *testing.T, s *fake.Server, cfg prgate.Config) *prgate.Report {
	t.Helper()
	rep, err := newGate(t, s).Evaluate(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func status(rep *prgate.Report, id string) prgate.Status {
	for _, f := range rep.Findings {
		if f.ID == id && f.Status != prgate.Pass {
			return f.Status
		}
	}
	for _, f := range rep.Findings {
		if f.ID == id {
			return f.Status
		}
	}
	return ""
}

func dump(rep *prgate.Report) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		b.WriteString("\n  " + string(f.Status) + " " + f.ID + " " + f.Detail)
	}
	return b.String()
}

func TestEvaluate_PassesWhenEveryConditionHolds(t *testing.T) {
	s := fake.New(t)
	rep := evaluate(t, s, config())
	if !rep.Passed {
		t.Fatalf("gate failed:%s", dump(rep))
	}
	for _, id := range prgate.FindingIDs {
		if status(rep, id) != prgate.Pass {
			t.Errorf("%s = %q, want PASS", id, status(rep, id))
		}
	}
	if len(s.Writes) != 0 {
		t.Errorf("Evaluate wrote: %v", s.Writes)
	}
}

func run(id int64, name, st, concl string) fake.Run {
	return fake.Run{ID: id, Name: name, Status: st, Conclusion: concl, App: fake.ActionsApp}
}

func done(id int64, name, concl string) fake.Run { return run(id, name, "completed", concl) }

func replaceRun(s *fake.Server, r fake.Run) {
	for i, cr := range s.CheckRuns {
		if cr.Name == r.Name {
			s.CheckRuns[i] = r
			return
		}
	}
	s.CheckRuns = append(s.CheckRuns, r)
}

func without(s *fake.Server, name string) {
	var out []fake.Run
	for _, cr := range s.CheckRuns {
		if cr.Name != name {
			out = append(out, cr)
		}
	}
	s.CheckRuns = out
}

// Each case breaks exactly one condition of the passing pull request; the
// gate must fail (or be undecided) with that condition's finding.
func TestEvaluate_FailsClosed(t *testing.T) {
	no := false
	tests := []struct {
		name   string
		mutate func(*fake.Server, *prgate.Config)
		id     string
		want   prgate.Status
	}{
		// pull request
		{"closed", func(s *fake.Server, _ *prgate.Config) { s.Pull.State = "closed" }, "pr.state", prgate.Fail},
		{"already merged", func(s *fake.Server, _ *prgate.Config) {
			s.Pull.State, s.Pull.Merged, s.Pull.MergeCommitSHA = "closed", true, fake.Merge
		}, "pr.state", prgate.Fail},
		{"unknown state", func(s *fake.Server, _ *prgate.Config) { s.Pull.State = "weird" }, "pr.state", prgate.Fail},
		{"draft", func(s *fake.Server, _ *prgate.Config) { s.Pull.Draft = true }, "pr.draft", prgate.Fail},
		{"head replaced", func(s *fake.Server, _ *prgate.Config) { s.Pull.HeadSHA = strings.Repeat("9", 40) }, "pr.head", prgate.Fail},
		{"base retargeted", func(s *fake.Server, _ *prgate.Config) { s.Pull.BaseRef = "release" }, "pr.base", prgate.Fail},
		{"not mergeable", func(s *fake.Server, _ *prgate.Config) { s.Pull.Mergeable, s.Pull.MergeableState = &no, "dirty" }, "pr.mergeable", prgate.Fail},
		{"blocked", func(s *fake.Server, _ *prgate.Config) { s.Pull.MergeableState = "blocked" }, "pr.mergeable", prgate.Fail},
		{"unstable", func(s *fake.Server, _ *prgate.Config) { s.Pull.MergeableState = "unstable" }, "pr.mergeable", prgate.Fail},
		{"behind (state)", func(s *fake.Server, _ *prgate.Config) { s.Pull.MergeableState = "behind" }, "pr.mergeable", prgate.Fail},
		{"has_hooks", func(s *fake.Server, _ *prgate.Config) { s.Pull.MergeableState = "has_hooks" }, "pr.mergeable", prgate.Fail},
		{"mergeable false but state clean", func(s *fake.Server, _ *prgate.Config) { s.Pull.Mergeable = &no }, "pr.mergeable", prgate.Fail},
		{"mergeable state unknown value", func(s *fake.Server, _ *prgate.Config) { s.Pull.MergeableState = "something-new" }, "pr.mergeable", prgate.Fail},
		{"mergeability never computed", func(s *fake.Server, _ *prgate.Config) { s.Pull.Mergeable, s.Pull.MergeableState = nil, "unknown" }, "pr.mergeable", prgate.Error},
		{"mergeable true but state unknown", func(s *fake.Server, _ *prgate.Config) { s.Pull.MergeableState = "unknown" }, "pr.mergeable", prgate.Error},
		{"pull 404", func(s *fake.Server, _ *prgate.Config) { s.Errors["pull"] = 404 }, "pr.read", prgate.Error},
		{"pull 500", func(s *fake.Server, _ *prgate.Config) { s.Errors["pull"] = 500 }, "pr.read", prgate.Error},
		{"pull rate limited", func(s *fake.Server, _ *prgate.Config) { s.Errors["pull"] = 403 }, "pr.read", prgate.Error},
		{"pull invalid JSON", func(s *fake.Server, _ *prgate.Config) { s.BadJSON["pull"] = true }, "pr.read", prgate.Error},

		// base
		{"base moved", func(s *fake.Server, _ *prgate.Config) { s.BaseRefSHA = strings.Repeat("8", 40) }, "base.sha", prgate.Fail},
		{"base ref not a commit", func(s *fake.Server, _ *prgate.Config) { s.BaseRefType = "tag" }, "base.sha", prgate.Error},
		{"base ref 500", func(s *fake.Server, _ *prgate.Config) { s.Errors["ref"] = 502 }, "base.sha", prgate.Error},
		{"behind base", func(s *fake.Server, _ *prgate.Config) { s.Compare.Status, s.Compare.Behind = "diverged", 1 }, "base.up_to_date", prgate.Fail},
		{"behind but status ahead", func(s *fake.Server, _ *prgate.Config) { s.Compare.Behind = 3 }, "base.up_to_date", prgate.Fail},
		{"identical to base", func(s *fake.Server, _ *prgate.Config) { s.Compare.Status, s.Compare.Ahead = "identical", 0 }, "base.up_to_date", prgate.Fail},
		{"compare 403", func(s *fake.Server, _ *prgate.Config) { s.Errors["compare"] = 403 }, "base.up_to_date", prgate.Error},

		// required checks
		{"rules 403", func(s *fake.Server, _ *prgate.Config) { s.Errors["rules"] = 403 }, "required.read", prgate.Error},
		{"rules 500", func(s *fake.Server, _ *prgate.Config) { s.Errors["rules"] = 500 }, "required.read", prgate.Error},
		{"rules 404", func(s *fake.Server, _ *prgate.Config) { s.Errors["rules"] = 404 }, "required.read", prgate.Error},
		{"rules invalid JSON", func(s *fake.Server, _ *prgate.Config) { s.BadJSON["rules"] = true }, "required.read", prgate.Error},
		{"rule without parameters", func(s *fake.Server, _ *prgate.Config) {
			s.Rules = `[{"type":"required_status_checks","ruleset_id":1}]`
		}, "required.read", prgate.Error},
		{"rule with unnamed check", func(s *fake.Server, _ *prgate.Config) {
			s.Rules = `[{"type":"required_status_checks","ruleset_id":1,"parameters":{"required_status_checks":[{"context":""}]}}]`
		}, "required.read", prgate.Error},
		{"branch 500", func(s *fake.Server, _ *prgate.Config) { s.Errors["branch"] = 500 }, "required.read", prgate.Error},
		{"protection hidden", func(s *fake.Server, _ *prgate.Config) { s.Branch = `{"name":"main","protected":true}` }, "required.read", prgate.Error},
		{"classic protection check missing", func(s *fake.Server, _ *prgate.Config) {
			s.Branch = `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"everyone","contexts":["e2e"],"checks":[{"context":"e2e","app_id":15368}]}}}`
		}, "checks.required", prgate.Fail},
		{"classic protection context missing", func(s *fake.Server, _ *prgate.Config) {
			s.Branch = `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"non_admins","contexts":["e2e"],"checks":[]}}}`
		}, "checks.required", prgate.Fail},
		{"no required checks anywhere", func(s *fake.Server, _ *prgate.Config) { s.Rules = `[]` }, "required.read", prgate.Fail},

		// check runs, statuses and workflow runs
		{"check runs 500", func(s *fake.Server, _ *prgate.Config) { s.Errors["check-runs"] = 500 }, "checks.read", prgate.Error},
		{"check runs rate limited", func(s *fake.Server, _ *prgate.Config) { s.Errors["check-runs"] = 403 }, "checks.read", prgate.Error},
		{"statuses 502", func(s *fake.Server, _ *prgate.Config) { s.Errors["status"] = 502 }, "checks.read", prgate.Error},
		{"statuses truncated", func(s *fake.Server, _ *prgate.Config) {
			s.Statuses, s.StatusTotal = []fake.Status{{Context: "ext", State: "success"}}, 2
		}, "checks.read", prgate.Error},
		{"workflow runs 500", func(s *fake.Server, _ *prgate.Config) { s.Errors["runs"] = 500 }, "checks.read", prgate.Error},
		{"zero checks", func(s *fake.Server, _ *prgate.Config) { s.CheckRuns = nil }, "checks.present", prgate.Fail},
		{"check in progress", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, run(11, "lint", "in_progress", "")) }, "checks.completed", prgate.Fail},
		{"check queued", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, run(11, "lint", "queued", "")) }, "checks.completed", prgate.Fail},
		{"check unknown status", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, run(11, "lint", "warping", "")) }, "checks.completed", prgate.Fail},
		{"workflow still running", func(s *fake.Server, _ *prgate.Config) {
			s.Workflows = append(s.Workflows, fake.WorkflowRun{ID: 101, WorkflowID: 2, Name: "Extended", Event: "pull_request", Status: "queued"})
		}, "checks.completed", prgate.Fail},
		{"status pending", func(s *fake.Server, _ *prgate.Config) { s.Statuses = []fake.Status{{Context: "ext", State: "pending"}} }, "checks.completed", prgate.Fail},
		{"failure", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "failure")) }, "checks.no_failure", prgate.Fail},
		{"cancelled", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "cancelled")) }, "checks.no_failure", prgate.Fail},
		{"timed_out", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "timed_out")) }, "checks.no_failure", prgate.Fail},
		{"action_required", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "action_required")) }, "checks.no_failure", prgate.Fail},
		{"stale", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "stale")) }, "checks.no_failure", prgate.Fail},
		{"startup_failure", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "startup_failure")) }, "checks.no_failure", prgate.Fail},
		{"unknown conclusion", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "glorious")) }, "checks.no_failure", prgate.Fail},
		{"completed without conclusion", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(11, "lint", "")) }, "checks.no_failure", prgate.Fail},
		{"status failure", func(s *fake.Server, _ *prgate.Config) { s.Statuses = []fake.Status{{Context: "ext", State: "failure"}} }, "checks.no_failure", prgate.Fail},
		{"status error", func(s *fake.Server, _ *prgate.Config) { s.Statuses = []fake.Status{{Context: "ext", State: "error"}} }, "checks.no_failure", prgate.Fail},
		{"status unknown state", func(s *fake.Server, _ *prgate.Config) { s.Statuses = []fake.Status{{Context: "ext", State: "weird"}} }, "checks.no_failure", prgate.Fail},
		{"workflow startup failure without check runs", func(s *fake.Server, _ *prgate.Config) {
			s.Workflows = append(s.Workflows, fake.WorkflowRun{ID: 101, WorkflowID: 2, Name: "Broken", Event: "pull_request", Status: "completed", Conclusion: "startup_failure"})
		}, "checks.no_failure", prgate.Fail},
		{"workflow cancelled", func(s *fake.Server, _ *prgate.Config) {
			s.Workflows[0].Conclusion = "cancelled"
		}, "checks.no_failure", prgate.Fail},

		// required checks against results
		{"required missing", func(s *fake.Server, _ *prgate.Config) { without(s, "test") }, "checks.required", prgate.Fail},
		{"required skipped", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(10, "test", "skipped")) }, "checks.required", prgate.Fail},
		{"required neutral", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(10, "test", "neutral")) }, "checks.required", prgate.Fail},
		{"required from another app", func(s *fake.Server, _ *prgate.Config) {
			replaceRun(s, fake.Run{ID: 10, Name: "test", Status: "completed", Conclusion: "success", App: 999})
		}, "checks.required", prgate.Fail},
		{"required only as a commit status while an app is required", func(s *fake.Server, _ *prgate.Config) {
			without(s, "test")
			s.Statuses = []fake.Status{{Context: "test", State: "success"}}
		}, "checks.required", prgate.Fail},
		{"named check missing", func(s *fake.Server, c *prgate.Config) { c.Require = []string{"e2e"} }, "checks.required", prgate.Fail},
		{"named check skipped", func(s *fake.Server, c *prgate.Config) {
			c.Require = []string{"extended"}
		}, "checks.required", prgate.Fail},
		{"named check as an erroring status", func(s *fake.Server, c *prgate.Config) {
			c.Require = []string{"ext"}
			s.Statuses = []fake.Status{{Context: "ext", State: "error"}}
		}, "checks.required", prgate.Fail},
		{"named check as a pending status", func(s *fake.Server, c *prgate.Config) {
			c.Require = []string{"ext"}
			s.Statuses = []fake.Status{{Context: "ext", State: "pending"}}
		}, "checks.required", prgate.Fail},
		{"required checks unreadable", func(s *fake.Server, _ *prgate.Config) { s.Errors["rules"] = 500 }, "checks.required", prgate.Error},

		// the CI gate
		{"ci-gate missing", func(s *fake.Server, _ *prgate.Config) { without(s, "ci-gate") }, "checks.ci_gate", prgate.Fail},
		{"ci-gate skipped", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(20, "ci-gate", "skipped")) }, "checks.ci_gate", prgate.Fail},
		{"ci-gate neutral", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(20, "ci-gate", "neutral")) }, "checks.ci_gate", prgate.Fail},
		{"ci-gate failure", func(s *fake.Server, _ *prgate.Config) { replaceRun(s, done(20, "ci-gate", "failure")) }, "checks.ci_gate", prgate.Fail},
		{"ci-gate from another app", func(s *fake.Server, _ *prgate.Config) {
			replaceRun(s, fake.Run{ID: 20, Name: "ci-gate", Status: "completed", Conclusion: "success", App: 999})
		}, "checks.ci_gate", prgate.Fail},
		{"ci-gate only as a commit status", func(s *fake.Server, _ *prgate.Config) {
			without(s, "ci-gate")
			s.Statuses = []fake.Status{{Context: "ci-gate", State: "success"}}
		}, "checks.ci_gate", prgate.Fail},
		{"only skipped results", func(s *fake.Server, _ *prgate.Config) {
			for i := range s.CheckRuns {
				s.CheckRuns[i].Conclusion = "skipped"
			}
		}, "checks.ci_gate", prgate.Fail},
		{"only neutral results", func(s *fake.Server, _ *prgate.Config) {
			for i := range s.CheckRuns {
				s.CheckRuns[i].Conclusion = "neutral"
			}
		}, "checks.ci_gate", prgate.Fail},

		// re-runs: the latest attempt decides
		{"old success, newer failure", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, done(30, "ci-gate", "failure"))
		}, "checks.ci_gate", prgate.Fail},
		{"old success, newer attempt running", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, run(30, "ci-gate", "in_progress", ""))
		}, "checks.ci_gate", prgate.Fail},
		{"old success, newer cancelled", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, done(30, "test", "cancelled"))
		}, "checks.required", prgate.Fail},
		{"same run listed twice with different outcomes", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, done(20, "ci-gate", "failure"))
		}, "checks.ci_gate", prgate.Fail},
		{"optional check listed twice with different outcomes", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, done(11, "lint", "skipped"))
		}, "checks.no_failure", prgate.Fail},
		{"required check listed twice with different outcomes", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, done(10, "test", "timed_out"))
		}, "checks.required", prgate.Fail},
		{"workflow re-run superseded by a newer failing run", func(s *fake.Server, _ *prgate.Config) {
			s.Workflows = append(s.Workflows, fake.WorkflowRun{ID: 150, WorkflowID: 1, Name: "CI", Event: "pull_request", Status: "completed", Conclusion: "failure"})
		}, "checks.no_failure", prgate.Fail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fake.New(t)
			cfg := config()
			tt.mutate(s, &cfg)
			rep := evaluate(t, s, cfg)
			if rep.Passed {
				t.Fatalf("gate passed:%s", dump(rep))
			}
			if got := status(rep, tt.id); got != tt.want {
				t.Fatalf("%s = %q, want %q:%s", tt.id, got, tt.want, dump(rep))
			}
			if len(s.Writes) != 0 {
				t.Errorf("Evaluate wrote: %v", s.Writes)
			}
		})
	}
}

// Situations that must pass: they guard against a gate so strict that it
// would be bypassed by hand.
func TestEvaluate_Passes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fake.Server, *prgate.Config)
	}{
		{"old failure, newer success", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append(s.CheckRuns, done(5, "ci-gate", "failure"), done(4, "test", "cancelled"))
		}},
		{"API order does not matter", func(s *fake.Server, _ *prgate.Config) {
			s.CheckRuns = append([]fake.Run{done(30, "ci-gate", "success"), done(5, "ci-gate", "failure")}, s.CheckRuns...)
		}},
		{"older workflow run cancelled by a newer successful one", func(s *fake.Server, _ *prgate.Config) {
			s.Workflows = append(s.Workflows, fake.WorkflowRun{ID: 90, WorkflowID: 1, Name: "CI", Event: "pull_request", Status: "completed", Conclusion: "cancelled"})
		}},
		{"no rules: the named checks decide", func(s *fake.Server, c *prgate.Config) {
			s.Rules = `[]`
			c.Require = []string{"test", "lint"}
		}},
		{"classic branch protection", func(s *fake.Server, _ *prgate.Config) {
			s.Rules = `[]`
			s.Branch = `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"everyone","contexts":["test"],"checks":[{"context":"test","app_id":15368}]}}}`
		}},
		{"successful commit status besides check runs", func(s *fake.Server, _ *prgate.Config) {
			s.Statuses = []fake.Status{{Context: "ext", State: "success"}}
		}},
		{"CI gate from any source", func(s *fake.Server, c *prgate.Config) {
			c.CIGateApp = 0
			without(s, "ci-gate")
			s.Statuses = []fake.Status{{Context: "ci-gate", State: "success"}}
			s.Rules = `[{"type":"required_status_checks","ruleset_id":1,"parameters":{"required_status_checks":[{"context":"test","integration_id":15368}]}}]`
		}},
		{"mergeability computed on the third read", func(s *fake.Server, _ *prgate.Config) {
			p := s.Pull
			p.Mergeable, p.MergeableState = nil, "unknown"
			s.PullSequence = []fake.Pull{p, p}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fake.New(t)
			cfg := config()
			tt.mutate(s, &cfg)
			rep := evaluate(t, s, cfg)
			if !rep.Passed {
				t.Fatalf("gate failed:%s", dump(rep))
			}
		})
	}
}

func TestEvaluate_MergeabilityReadsAreBounded(t *testing.T) {
	s := fake.New(t)
	s.Pull.Mergeable, s.Pull.MergeableState = nil, "unknown"
	cfg := config()
	cfg.MergeableReads = 4
	rep := evaluate(t, s, cfg)
	if rep.Passed || status(rep, "pr.mergeable") != prgate.Error {
		t.Fatalf("want pr.mergeable ERROR:%s", dump(rep))
	}
	if s.PullReads != 4 {
		t.Errorf("pull request read %d times, want 4", s.PullReads)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*prgate.Config)
	}{
		{"short head", func(c *prgate.Config) { c.HeadSHA = fake.Head[:7] }},
		{"uppercase head", func(c *prgate.Config) { c.HeadSHA = strings.ToUpper(strings.Repeat("a", 40)) }},
		{"empty base sha", func(c *prgate.Config) { c.BaseSHA = "" }},
		{"base as a ref", func(c *prgate.Config) { c.BaseRef = "refs/heads/main" }},
		{"no number", func(c *prgate.Config) { c.Number = 0 }},
		{"no CI gate", func(c *prgate.Config) { c.CIGate = " " }},
		{"empty named check", func(c *prgate.Config) { c.Require = []string{""} }},
		{"bad repository", func(c *prgate.Config) { c.Repo = "a/b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config()
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
			s := fake.New(t)
			if _, err := newGate(t, s).Evaluate(context.Background(), c); err == nil {
				t.Fatal("Evaluate accepted it")
			}
			if s.PullReads != 0 {
				t.Error("Evaluate called the API with an invalid configuration")
			}
		})
	}
}

func TestReport_MissingFindingFails(t *testing.T) {
	// A report passes only with a PASS for every listed condition; this
	// keeps a forgotten condition from passing silently.
	if len(prgate.FindingIDs) != 15 {
		t.Fatalf("FindingIDs has %d entries; update this test when adding a condition", len(prgate.FindingIDs))
	}
	s := fake.New(t)
	s.Errors["pull"] = 500 // pr.* after pr.read are never reported
	rep := evaluate(t, s, config())
	if rep.Passed {
		t.Fatal("passed without pull request findings")
	}
}
