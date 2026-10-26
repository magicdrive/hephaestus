package preflight

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/github"
)

// Several check runs of a commit can share a name: re-run attempts, the
// same workflow run again (a release re-run, a second tag on the same
// commit), or another app reporting a check with that name. The verdict
// must not depend on the order the API returns them in, so runs are
// grouped by (app, name) and each group is decided by its latest run:
//
//   - a run is newer than another when its ID is higher (IDs grow with
//     creation: a re-run attempt gets a new, higher ID); when an ID is
//     missing, by started_at;
//   - when neither tells two runs apart and their outcomes differ, the group
//     is ambiguous and the check is UNKNOWN, never guessed;
//   - a run that is not completed is "pending", so a newer pending run is
//     never hidden behind an older success, and an older failure does not
//     outlive a newer success.
//
// Runs of different apps are different checks: each must pass on its own,
// and a required check that names an app (integration_id) is satisfied
// only by that app's runs.

// checkKey identifies a group of check runs.
type checkKey struct {
	name string
	app  int64 // 0 when the run has no app
}

// checkGroup is a check name of one app and its latest run.
type checkGroup struct {
	key        checkKey
	appSlug    string
	latest     github.CheckRun
	outcome    string // the latest run's conclusion, or "pending"
	superseded int    // older runs in the group
	ambiguous  bool   // the latest run could not be determined
}

func runOutcome(r github.CheckRun) string {
	if r.Status != "completed" {
		return "pending"
	}
	if r.Conclusion == "" {
		return "unknown"
	}
	return r.Conclusion
}

// newer reports whether a is newer than b, and whether that is decidable.
func newer(a, b github.CheckRun) (isNewer, decided bool) {
	if a.ID != 0 && b.ID != 0 && a.ID != b.ID {
		return a.ID > b.ID, true
	}
	if a.StartedAt != nil && b.StartedAt != nil && !a.StartedAt.Equal(*b.StartedAt) {
		return a.StartedAt.After(*b.StartedAt), true
	}
	return false, false
}

// latestCheckRuns groups runs by app and name and picks each group's latest
// run, independently of the input order. Groups are returned sorted by
// name, then app.
func latestCheckRuns(runs []github.CheckRun) []checkGroup {
	byKey := map[checkKey][]github.CheckRun{}
	for _, r := range runs {
		k := checkKey{name: r.Name}
		if r.App != nil {
			k.app = r.App.ID
		}
		byKey[k] = append(byKey[k], r)
	}
	var out []checkGroup
	for k, rs := range byKey {
		// A canonical order first, so that ties are handled the same way
		// whatever the API order was.
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].ID != rs[j].ID {
				return rs[i].ID < rs[j].ID
			}
			return runOutcome(rs[i]) < runOutcome(rs[j])
		})
		best := rs[0]
		for _, r := range rs[1:] {
			if n, ok := newer(r, best); ok && n {
				best = r
			}
		}
		g := checkGroup{key: k, latest: best, outcome: runOutcome(best), superseded: len(rs) - 1}
		if best.App != nil {
			g.appSlug = best.App.Slug
		}
		for _, r := range rs {
			if _, ok := newer(r, best); !ok && runOutcome(r) != g.outcome {
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

func (g checkGroup) label() string {
	if g.appSlug != "" {
		return g.key.name + " [" + g.appSlug + "]"
	}
	if g.key.app != 0 {
		return fmt.Sprintf("%s [app %d]", g.key.name, g.key.app)
	}
	return g.key.name
}

func (g checkGroup) describe() string {
	s := fmt.Sprintf("%s: %s (run %d", g.label(), g.outcome, g.latest.ID)
	if g.superseded > 0 {
		s += fmt.Sprintf(", %d older superseded", g.superseded)
	}
	if g.ambiguous {
		s += ", ambiguous"
	}
	return s + ")"
}

// requiredCheck is a status check a ruleset requires; app 0 accepts any
// source.
type requiredCheck struct {
	context string
	app     int64
}

func (r requiredCheck) String() string {
	if r.app == 0 {
		return r.context
	}
	return fmt.Sprintf("%s (app %d)", r.context, r.app)
}

// requiredOutcome evaluates a required check against the latest runs and
// the commit statuses: "success", "missing", "ambiguous" or the failing or
// pending outcome. Every matching group must succeed.
func requiredOutcome(req requiredCheck, groups []checkGroup, statuses map[string]string) (string, string) {
	var matched []checkGroup
	var otherApps []string
	for _, g := range groups {
		if g.key.name != req.context {
			continue
		}
		if req.app != 0 && g.key.app != req.app {
			otherApps = append(otherApps, g.label())
			continue
		}
		matched = append(matched, g)
	}
	if st, ok := statuses[req.context]; ok && req.app == 0 {
		matched = append(matched, checkGroup{key: checkKey{name: req.context}, outcome: st})
	}
	if len(matched) == 0 {
		if len(otherApps) > 0 {
			return "missing", fmt.Sprintf("%s: reported only by %s", req, strings.Join(otherApps, ", "))
		}
		return "missing", req.String() + ": not reported"
	}
	for _, g := range matched {
		if g.ambiguous {
			return "ambiguous", g.describe()
		}
		if g.outcome != "success" {
			return g.outcome, g.describe()
		}
	}
	return "success", ""
}

// requiredChecks lists the status checks the active branch rulesets require
// for branch, deduplicated and sorted. defaultBranch is the repository's
// default branch (~DEFAULT_BRANCH), which the release branch need not be.
func requiredChecks(sets []github.Ruleset, branch, defaultBranch string) []requiredCheck {
	var out []requiredCheck
	for _, rs := range activeCovering(sets, "branch", "refs/heads/"+branch, defaultBranch) {
		for _, rule := range rs.Rules {
			for _, c := range rule.Parameters.RequiredStatusChecks {
				rc := requiredCheck{context: c.Context, app: c.IntegrationID}
				if !slices.Contains(out, rc) {
					out = append(out, rc)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].context != out[j].context {
			return out[i].context < out[j].context
		}
		return out[i].app < out[j].app
	})
	return out
}
