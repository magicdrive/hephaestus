package prgate

import (
	"fmt"
	"sort"

	"github.com/magicdrive/hephaestus/internal/github"
)

// The latest-run rule is the one internal/preflight uses for the release
// CI check (checkruns.go there); it is repeated here because the gate's
// verdicts differ: an ambiguous group fails instead of being UNKNOWN, and a
// required check accepts only "success".

const outcomePending = "pending"

type runKey struct {
	name string
	app  int64
}

// runGroup is the check runs of one name and app, decided by the latest.
type runGroup struct {
	key        runKey
	latest     github.CheckRun
	outcome    string // the latest run's conclusion, "pending" or "unknown"
	superseded int
	ambiguous  bool
}

func runOutcome(r github.CheckRun) string {
	if r.Status != "completed" {
		return outcomePending
	}
	if r.Conclusion == "" {
		return "unknown"
	}
	return r.Conclusion
}

// newerRun reports whether a is newer than b, and whether that is decidable
// (IDs grow with creation: a re-run attempt has a higher ID).
func newerRun(a, b github.CheckRun) (isNewer, decided bool) {
	if a.ID != 0 && b.ID != 0 && a.ID != b.ID {
		return a.ID > b.ID, true
	}
	if a.StartedAt != nil && b.StartedAt != nil && !a.StartedAt.Equal(*b.StartedAt) {
		return a.StartedAt.After(*b.StartedAt), true
	}
	return false, false
}

// latestRuns groups runs by (name, app) and picks each group's latest run,
// independently of the order the API returned them in.
func latestRuns(runs []github.CheckRun) []runGroup {
	byKey := map[runKey][]github.CheckRun{}
	for _, r := range runs {
		k := runKey{name: r.Name}
		if r.App != nil {
			k.app = r.App.ID
		}
		byKey[k] = append(byKey[k], r)
	}
	out := make([]runGroup, 0, len(byKey))
	for k, rs := range byKey {
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].ID != rs[j].ID {
				return rs[i].ID < rs[j].ID
			}
			return runOutcome(rs[i]) < runOutcome(rs[j])
		})
		best := rs[0]
		for _, r := range rs[1:] {
			if n, ok := newerRun(r, best); ok && n {
				best = r
			}
		}
		g := runGroup{key: k, latest: best, outcome: runOutcome(best), superseded: len(rs) - 1}
		for _, r := range rs {
			if _, ok := newerRun(r, best); !ok && runOutcome(r) != g.outcome {
				g.ambiguous = true
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.name != out[j].key.name {
			return out[i].key.name < out[j].key.name
		}
		return out[i].key.app < out[j].key.app
	})
	return out
}

func (g runGroup) describe() string {
	s := fmt.Sprintf("%s [app %d]: %s (run %d", g.key.name, g.key.app, g.outcome, g.latest.ID)
	if g.superseded > 0 {
		s += fmt.Sprintf(", %d older superseded", g.superseded)
	}
	return s + ")"
}

type wfKey struct {
	workflow int64
	name     string
	event    string
}

type wfGroup struct {
	key       wfKey
	run       github.WorkflowRun
	ambiguous bool
}

func (w wfGroup) describe() string {
	c := w.run.Conclusion
	if w.run.Status != "completed" {
		c = w.run.Status
	}
	return fmt.Sprintf("workflow %q (%s) run %d attempt %d: %s", w.key.name, w.key.event, w.run.ID, w.run.RunAttempt, c)
}

// latestWorkflowRuns keeps the latest run of each (workflow, event): a
// newer run of the same workflow for the same commit supersedes an older
// one (for example one cancelled by a concurrency group). A re-run attempt
// is the same run object, already reporting its latest attempt.
func latestWorkflowRuns(runs []github.WorkflowRun) []wfGroup {
	by := map[wfKey][]github.WorkflowRun{}
	for _, r := range runs {
		k := wfKey{workflow: r.WorkflowID, name: r.Name, event: r.Event}
		by[k] = append(by[k], r)
	}
	out := make([]wfGroup, 0, len(by))
	for k, rs := range by {
		best := rs[0]
		ambiguous := false
		for _, r := range rs[1:] {
			switch {
			case r.ID > best.ID:
				best = r
				ambiguous = false
			case r.ID == best.ID && (r.Status != best.Status || r.Conclusion != best.Conclusion):
				ambiguous = true
			}
		}
		if best.ID == 0 {
			ambiguous = true
		}
		out = append(out, wfGroup{key: k, run: best, ambiguous: ambiguous})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].describe() < out[j].describe() })
	return out
}
