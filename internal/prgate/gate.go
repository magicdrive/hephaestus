// Package prgate decides whether a pull request may be merged, and merges
// it only when every condition holds. It is the engine of cmd/pr-gate.
//
// Every condition fails closed: an API error, a value the gate does not
// know, a missing check or an undecidable "latest run" is a failure, never
// a pass. The conditions (finding IDs in parentheses):
//
//   - the pull request is open, not merged and not a draft (pr.state,
//     pr.draft);
//   - its head is exactly the expected commit (pr.head) and its base is the
//     expected branch (pr.base);
//   - the base branch is exactly at the expected commit (base.sha) and the
//     head contains it: behind by 0 (base.up_to_date);
//   - GitHub reports it mergeable and clean; while GitHub is still
//     computing it (null / unknown) it is re-read a bounded number of times
//     (pr.mergeable);
//   - the required checks could be read: active branch rules
//     (required_status_checks) and classic branch protection, plus the
//     checks named by the caller and the CI gate; an empty set fails
//     (required.read);
//   - the check runs, commit statuses and workflow runs of the head could
//     be read completely (checks.read), there is at least one (checks.present),
//     all are completed (checks.completed) and none failed or has an unknown
//     state (checks.no_failure);
//   - every required check is satisfied by a success of its latest run;
//     skipped or neutral is not success (checks.required); the CI gate
//     check is required and must succeed (checks.ci_gate).
//
// Re-runs: check runs are grouped by (name, app) and each group is decided
// by its latest run (highest ID; when IDs cannot order two runs whose
// outcomes differ, the group is ambiguous and fails). An older success never
// hides a newer pending or failed attempt. Workflow runs are grouped by
// (workflow, event) the same way.
//
// Merging (Merge) evaluates everything again, re-reads the pull request and
// the base branch once more, and then sends one conditional merge request
// pinned to the expected head (PUT pulls/{n}/merge with "sha"), never
// retried. Afterwards it verifies that the merge commit's parents are the
// expected base and head: the merge API cannot pin the base, so a base
// moving in the last moment is detected (and reported as an alarm) rather
// than prevented.
package prgate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
)

// Defaults for Config.
const (
	DefaultCIGate = "ci-gate"
	// GitHubActionsAppID is the app ID of GitHub Actions on github.com:
	// by default the CI gate must be a check run of GitHub Actions, not a
	// commit status or another app's check of the same name.
	GitHubActionsAppID   = 15368
	DefaultMergeableRead = 6
	DefaultMergeableWait = 10 * time.Second
)

// Config is what the caller expects of the pull request.
type Config struct {
	Owner, Repo string
	Number      int
	// HeadSHA and BaseSHA are full 40-character commit SHAs; BaseRef is the
	// base branch name.
	HeadSHA, BaseRef, BaseSHA string
	// CIGate is the name of the check that aggregates CI (required, must
	// succeed). CIGateApp is the app that must report it; 0 accepts any
	// source, including a commit status.
	CIGate    string
	CIGateApp int64
	// Require names checks that must succeed in addition to the branch's
	// required checks (any app). For a repository without required checks
	// this is the set the gate decides by.
	Require []string
	// MergeableReads bounds the reads of the pull request while GitHub has
	// not computed its mergeability yet; MergeableWait is the wait between
	// them.
	MergeableReads int
	MergeableWait  time.Duration
}

func (c *Config) defaults() {
	if c.MergeableReads <= 0 {
		c.MergeableReads = DefaultMergeableRead
	}
	if c.MergeableWait <= 0 {
		c.MergeableWait = DefaultMergeableWait
	}
}

// Validate checks the configuration itself.
func (c *Config) Validate() error {
	var errs []error
	if c.Owner == "" || c.Repo == "" || strings.ContainsAny(c.Owner+c.Repo, "/\\?#% ") {
		errs = append(errs, fmt.Errorf("invalid repository %q", c.Owner+"/"+c.Repo))
	}
	if c.Number <= 0 {
		errs = append(errs, fmt.Errorf("invalid pull request number %d", c.Number))
	}
	if !github.IsFullSHA(c.HeadSHA) {
		errs = append(errs, fmt.Errorf("expected head %q is not a full 40-character lowercase commit SHA", c.HeadSHA))
	}
	if !github.IsFullSHA(c.BaseSHA) {
		errs = append(errs, fmt.Errorf("expected base %q is not a full 40-character lowercase commit SHA", c.BaseSHA))
	}
	if c.BaseRef == "" || strings.HasPrefix(c.BaseRef, "refs/") || strings.Contains(c.BaseRef, "..") {
		errs = append(errs, fmt.Errorf("invalid base branch %q (give the branch name, e.g. main)", c.BaseRef))
	}
	if strings.TrimSpace(c.CIGate) == "" {
		errs = append(errs, errors.New("the CI gate check name is empty"))
	}
	if c.CIGateApp < 0 {
		errs = append(errs, fmt.Errorf("invalid CI gate app %d", c.CIGateApp))
	}
	for _, r := range c.Require {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, errors.New("empty required check name"))
		}
	}
	return errors.Join(errs...)
}

// Status is the result of one condition.
type Status string

// Statuses. Error means the condition could not be decided (API failure,
// unknown value); it fails the gate like Fail.
const (
	Pass  Status = "PASS"
	Fail  Status = "FAIL"
	Error Status = "ERROR"
)

// Finding is the result of one condition.
type Finding struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the gate's verdict.
type Report struct {
	Repository string    `json:"repository"`
	Number     int       `json:"number"`
	HeadSHA    string    `json:"head_sha"`
	BaseRef    string    `json:"base_ref"`
	BaseSHA    string    `json:"base_sha"`
	Required   []string  `json:"required"`
	Findings   []Finding `json:"findings"`
	// Passed is true only when every finding is Pass and none is missing.
	Passed bool `json:"passed"`
}

// FindingIDs lists every condition, in report order. A report passes only
// when it has a Pass finding for each of them.
var FindingIDs = []string{
	"pr.read", "pr.state", "pr.draft", "pr.head", "pr.base", "pr.mergeable",
	"base.sha", "base.up_to_date",
	"required.read",
	"checks.read", "checks.present", "checks.completed", "checks.no_failure",
	"checks.required", "checks.ci_gate",
}

func (r *Report) add(id string, s Status, format string, args ...any) {
	r.Findings = append(r.Findings, Finding{ID: id, Status: s, Detail: fmt.Sprintf(format, args...)})
}

func (r *Report) finish() {
	got := map[string]Status{}
	for _, f := range r.Findings {
		if prev, ok := got[f.ID]; !ok || prev == Pass {
			got[f.ID] = f.Status
		}
	}
	r.Passed = true
	for _, id := range FindingIDs {
		if got[id] != Pass {
			r.Passed = false
		}
	}
	for _, f := range r.Findings {
		if f.Status != Pass {
			r.Passed = false
		}
	}
}

// Failed returns the findings that are not Pass.
func (r *Report) Failed() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Status != Pass {
			out = append(out, f)
		}
	}
	return out
}

// Gate evaluates and merges pull requests with a GitHub client.
type Gate struct {
	GH *github.Client
}

// Evaluate checks every condition and never writes. An invalid
// configuration is returned as an error; everything else is in the report.
func (g *Gate) Evaluate(ctx context.Context, cfg Config) (*Report, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.defaults()
	r := &Report{Repository: cfg.Owner + "/" + cfg.Repo, Number: cfg.Number, HeadSHA: cfg.HeadSHA, BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA}
	g.checkPull(ctx, cfg, r)
	g.checkBase(ctx, cfg, r)
	required, ok := g.requiredChecks(ctx, cfg, r)
	g.checkChecks(ctx, cfg, r, required, ok)
	r.finish()
	return r, nil
}

func (g *Gate) readPull(ctx context.Context, cfg Config) (*github.Pull, int, error) {
	var p *github.Pull
	reads := 0
	for {
		var err error
		p, err = g.GH.GetPull(ctx, cfg.Owner, cfg.Repo, cfg.Number)
		reads++
		if err != nil {
			return nil, reads, err
		}
		computed := p.Mergeable != nil && p.MergeableState != "" && p.MergeableState != "unknown"
		// A closed or draft pull request will not become mergeable.
		if computed || p.State != "open" || p.Draft || reads >= cfg.MergeableReads {
			return p, reads, nil
		}
		if err := g.GH.Wait(ctx, cfg.MergeableWait); err != nil {
			return nil, reads, err
		}
	}
}

func (g *Gate) checkPull(ctx context.Context, cfg Config, r *Report) {
	p, reads, err := g.readPull(ctx, cfg)
	if err != nil {
		r.add("pr.read", Error, "cannot read pull request #%d: %v", cfg.Number, err)
		return
	}
	r.add("pr.read", Pass, "%s (%d read(s))", p.HTMLURL, reads)

	switch {
	case p.State == "open" && !p.Merged:
		r.add("pr.state", Pass, "open")
	case p.Merged:
		r.add("pr.state", Fail, "already merged (%s)", p.MergeCommitSHA)
	default:
		r.add("pr.state", Fail, "state is %q, not open", p.State)
	}
	if p.Draft {
		r.add("pr.draft", Fail, "draft pull request")
	} else {
		r.add("pr.draft", Pass, "ready for review")
	}
	if p.Head.SHA == cfg.HeadSHA {
		r.add("pr.head", Pass, "head %s", p.Head.SHA)
	} else {
		r.add("pr.head", Fail, "head is %s, expected %s (pushed again?)", p.Head.SHA, cfg.HeadSHA)
	}
	if p.Base.Ref == cfg.BaseRef {
		r.add("pr.base", Pass, "base %s", p.Base.Ref)
	} else {
		r.add("pr.base", Fail, "base is %q, expected %q", p.Base.Ref, cfg.BaseRef)
	}
	switch {
	case p.Mergeable == nil || p.MergeableState == "" || p.MergeableState == "unknown":
		r.add("pr.mergeable", Error, "GitHub has not computed mergeability after %d read(s) (mergeable=%s, mergeable_state=%q)", reads, fmtBool(p.Mergeable), p.MergeableState)
	case !*p.Mergeable:
		r.add("pr.mergeable", Fail, "not mergeable (mergeable_state=%q)", p.MergeableState)
	case p.MergeableState != "clean":
		r.add("pr.mergeable", Fail, "mergeable_state is %q, not clean", p.MergeableState)
	default:
		r.add("pr.mergeable", Pass, "mergeable, clean")
	}
}

func fmtBool(b *bool) string {
	if b == nil {
		return "null"
	}
	return fmt.Sprint(*b)
}

func (g *Gate) checkBase(ctx context.Context, cfg Config, r *Report) {
	ref, err := g.GH.GetRef(ctx, cfg.Owner, cfg.Repo, "heads/"+cfg.BaseRef)
	switch {
	case err != nil:
		r.add("base.sha", Error, "cannot read refs/heads/%s: %v", cfg.BaseRef, err)
	case ref.Object.Type != "commit":
		r.add("base.sha", Error, "refs/heads/%s points to a %q, not a commit", cfg.BaseRef, ref.Object.Type)
	case ref.Object.SHA != cfg.BaseSHA:
		r.add("base.sha", Fail, "%s is at %s, expected %s (the base moved)", cfg.BaseRef, ref.Object.SHA, cfg.BaseSHA)
	default:
		r.add("base.sha", Pass, "%s at %s", cfg.BaseRef, cfg.BaseSHA)
	}
	// Compared by commit, never by branch name: the branch could move
	// between this call and the one above.
	cmp, err := g.GH.CompareCommits(ctx, cfg.Owner, cfg.Repo, cfg.BaseSHA, cfg.HeadSHA)
	switch {
	case err != nil:
		r.add("base.up_to_date", Error, "cannot compare %s...%s: %v", short(cfg.BaseSHA), short(cfg.HeadSHA), err)
	case cmp.BehindBy != 0:
		r.add("base.up_to_date", Fail, "head is %d commit(s) behind %s (status %s)", cmp.BehindBy, short(cfg.BaseSHA), cmp.Status)
	case cmp.Status != github.CompareAhead:
		r.add("base.up_to_date", Fail, "head is %q relative to the base, expected %q", cmp.Status, github.CompareAhead)
	default:
		r.add("base.up_to_date", Pass, "head is %d commit(s) ahead of %s and 0 behind", cmp.AheadBy, short(cfg.BaseSHA))
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// requirement is a check that must succeed. app 0 accepts any source.
type requirement struct {
	context string
	app     int64
	source  string
}

func (q requirement) String() string {
	from := "any source"
	if q.app != 0 {
		from = fmt.Sprintf("app %d", q.app)
	}
	if q.source == "" {
		return fmt.Sprintf("%s (%s)", q.context, from)
	}
	return fmt.Sprintf("%s (%s; %s)", q.context, from, q.source)
}

func addRequirement(list []requirement, q requirement) []requirement {
	for i, e := range list {
		if e.context == q.context && e.app == q.app {
			if !strings.Contains(e.source, q.source) {
				list[i].source += ", " + q.source
			}
			return list
		}
	}
	return append(list, q)
}

func (g *Gate) requiredChecks(ctx context.Context, cfg Config, r *Report) ([]requirement, bool) {
	var req []requirement
	var errs []string
	rules, err := g.GH.GetBranchRules(ctx, cfg.Owner, cfg.Repo, cfg.BaseRef)
	if err != nil {
		errs = append(errs, fmt.Sprintf("branch rules: %v", err))
	}
	fromRules := 0
	for _, rule := range rules {
		checks, _, err := rule.RequiredStatusChecks()
		if err != nil {
			errs = append(errs, fmt.Sprintf("ruleset %d: %v", rule.RulesetID, err))
			continue
		}
		for _, c := range checks {
			if c.Context == "" {
				errs = append(errs, fmt.Sprintf("ruleset %d: required check without a name", rule.RulesetID))
				continue
			}
			fromRules++
			req = addRequirement(req, requirement{context: c.Context, app: c.IntegrationID, source: fmt.Sprintf("ruleset %d", rule.RulesetID)})
		}
	}
	b, err := g.GH.GetBranch(ctx, cfg.Owner, cfg.Repo, cfg.BaseRef)
	fromProtection := 0
	switch {
	case err != nil:
		errs = append(errs, fmt.Sprintf("branch protection: %v", err))
	case b.Protected && b.Protection == nil:
		errs = append(errs, "branch protection: the branch is protected but its protection is not visible to this token")
	case b.Protection != nil && b.Protection.RequiredStatusChecks != nil && b.Protection.RequiredStatusChecks.EnforcementLevel != "off":
		rs := b.Protection.RequiredStatusChecks
		for _, c := range rs.Checks {
			var app int64
			if c.AppID != nil {
				app = *c.AppID
			}
			fromProtection++
			req = addRequirement(req, requirement{context: c.Context, app: app, source: "branch protection"})
		}
		for _, name := range rs.Contexts {
			if !slices.ContainsFunc(rs.Checks, func(c github.ProtectionCheck) bool {
				return c.Context == name
			}) {
				fromProtection++
				req = addRequirement(req, requirement{context: name, source: "branch protection"})
			}
		}
	}
	for _, name := range cfg.Require {
		req = addRequirement(req, requirement{context: name, source: "-require"})
	}
	req = addRequirement(req, requirement{context: cfg.CIGate, app: cfg.CIGateApp, source: "CI gate"})
	sort.Slice(req, func(i, j int) bool {
		if req[i].context != req[j].context {
			return req[i].context < req[j].context
		}
		return req[i].app < req[j].app
	})
	for _, q := range req {
		r.Required = append(r.Required, q.String())
	}
	switch {
	case len(errs) > 0:
		r.add("required.read", Error, "cannot read the required checks of %s: %s", cfg.BaseRef, strings.Join(errs, "; "))
		return req, false
	case fromRules+fromProtection == 0 && len(cfg.Require) == 0:
		r.add("required.read", Fail, "%s has no required status checks (rulesets or branch protection): name the checks that must pass with -require", cfg.BaseRef)
		return req, false
	}
	r.add("required.read", Pass, "%d from rulesets, %d from branch protection, %d named by the caller, plus the CI gate", fromRules, fromProtection, len(cfg.Require))
	return req, true
}

func (g *Gate) checkChecks(ctx context.Context, cfg Config, r *Report, required []requirement, requiredOK bool) {
	runs, err1 := g.GH.ListCheckRuns(ctx, cfg.Owner, cfg.Repo, cfg.HeadSHA)
	status, err2 := g.GH.GetCombinedStatus(ctx, cfg.Owner, cfg.Repo, cfg.HeadSHA)
	wfs, err3 := g.GH.ListWorkflowRunsForSHA(ctx, cfg.Owner, cfg.Repo, cfg.HeadSHA)
	var errs []string
	for _, e := range []struct {
		what string
		err  error
	}{{"check runs", err1}, {"commit statuses", err2}, {"workflow runs", err3}} {
		if e.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.what, e.err))
		}
	}
	if err2 == nil && status.TotalCount != len(status.Statuses) {
		errs = append(errs, fmt.Sprintf("commit statuses: %d of %d listed", len(status.Statuses), status.TotalCount))
	}
	if len(errs) > 0 {
		r.add("checks.read", Error, "cannot read the checks of %s: %s", short(cfg.HeadSHA), strings.Join(errs, "; "))
		return
	}
	groups := latestRuns(runs)
	statuses := map[string]string{}
	for _, s := range status.Statuses {
		if _, seen := statuses[s.Context]; !seen { // newest first
			statuses[s.Context] = s.State
		}
	}
	latestWF := latestWorkflowRuns(wfs)
	r.add("checks.read", Pass, "%d check run(s) in %d check(s), %d commit status(es), %d workflow run(s)", len(runs), len(groups), len(statuses), len(latestWF))

	if len(groups)+len(statuses) == 0 {
		r.add("checks.present", Fail, "no check runs or commit statuses on %s", short(cfg.HeadSHA))
	} else {
		r.add("checks.present", Pass, "%d check(s) and %d status(es)", len(groups), len(statuses))
	}

	var pending, failed []string
	for _, gr := range groups {
		switch {
		case gr.ambiguous:
			failed = append(failed, gr.describe()+": cannot tell which run is the latest")
		case gr.outcome == outcomePending:
			pending = append(pending, gr.describe())
		case !acceptedConclusion(gr.outcome):
			failed = append(failed, gr.describe())
		}
	}
	for ctxName, st := range statuses {
		switch st {
		case "success":
		case "pending":
			pending = append(pending, "status "+ctxName)
		default:
			failed = append(failed, fmt.Sprintf("status %s: %s", ctxName, st))
		}
	}
	for _, w := range latestWF {
		switch {
		case w.ambiguous:
			failed = append(failed, w.describe()+": cannot tell which run is the latest")
		case w.run.Status != "completed":
			pending = append(pending, w.describe())
		case !acceptedConclusion(w.run.Conclusion):
			// A workflow that fails before starting a job (an invalid
			// file: startup_failure) has no check runs to show it.
			failed = append(failed, w.describe())
		}
	}
	sort.Strings(pending)
	sort.Strings(failed)
	if len(pending) > 0 {
		r.add("checks.completed", Fail, "not completed: %s", strings.Join(pending, "; "))
	} else {
		r.add("checks.completed", Pass, "every check and workflow run completed")
	}
	if len(failed) > 0 {
		r.add("checks.no_failure", Fail, "failed or unknown: %s", strings.Join(failed, "; "))
	} else {
		r.add("checks.no_failure", Pass, "no failed, cancelled, timed out, stale or unknown result")
	}

	if !requiredOK {
		r.add("checks.required", Error, "the required checks are unknown (see required.read)")
	} else {
		var bad []string
		for _, q := range required {
			if q.context == cfg.CIGate && q.app == cfg.CIGateApp {
				continue // checks.ci_gate
			}
			if ok, why := satisfied(q, groups, statuses); !ok {
				bad = append(bad, why)
			}
		}
		if len(bad) > 0 {
			r.add("checks.required", Fail, "%s", strings.Join(bad, "; "))
		} else {
			r.add("checks.required", Pass, "%d required check(s) succeeded", len(required))
		}
	}
	if ok, why := satisfied(requirement{context: cfg.CIGate, app: cfg.CIGateApp}, groups, statuses); ok {
		r.add("checks.ci_gate", Pass, "%s succeeded", cfg.CIGate)
	} else {
		r.add("checks.ci_gate", Fail, "%s", why)
	}
}

// acceptedConclusion reports whether a completed result is not a failure.
// skipped and neutral are accepted for checks nobody requires; a required
// check must be "success" (satisfied).
func acceptedConclusion(c string) bool {
	switch c {
	case "success", "skipped", "neutral":
		return true
	}
	return false
}

// satisfied reports whether q is met: at least one matching result, and
// every matching latest result is exactly "success".
func satisfied(q requirement, groups []runGroup, statuses map[string]string) (bool, string) {
	var matched []string
	var others []string
	for _, gr := range groups {
		if gr.key.name != q.context {
			continue
		}
		if q.app != 0 && gr.key.app != q.app {
			others = append(others, gr.describe())
			continue
		}
		if gr.ambiguous {
			return false, gr.describe() + ": cannot tell which run is the latest"
		}
		if gr.outcome != "success" {
			return false, fmt.Sprintf("%s: required, but %s", gr.describe(), gr.outcome)
		}
		matched = append(matched, gr.describe())
	}
	if st, ok := statuses[q.context]; ok {
		if q.app != 0 {
			others = append(others, "commit status "+q.context)
		} else {
			if st != "success" {
				return false, fmt.Sprintf("status %s: required, but %s", q.context, st)
			}
			matched = append(matched, "status "+q.context)
		}
	}
	if len(matched) == 0 {
		if len(others) > 0 {
			return false, fmt.Sprintf("%s: missing (reported only by %s)", q, strings.Join(others, ", "))
		}
		return false, fmt.Sprintf("%s: missing", q)
	}
	return true, ""
}

// MergeOutcome is the result of Merge.
type MergeOutcome struct {
	Report *Report `json:"report"`
	// Attempted is true when the merge request was sent.
	Attempted bool `json:"attempted"`
	// Merged is true when GitHub merged the pull request.
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	// Verified is true when the merge commit's parents are exactly the
	// expected base and head (merge method "merge") or its first parent is
	// the expected base ("squash").
	Verified bool `json:"verified"`
	// Unknown is true when the merge request was sent but whether it
	// merged could not be established.
	Unknown bool   `json:"unknown"`
	Problem string `json:"problem,omitempty"`
}

// Merge evaluates every condition and, only when all pass, merges the pull
// request pinned to the expected head, then verifies the result. method is
// "merge" or "squash".
func (g *Gate) Merge(ctx context.Context, cfg Config, method string) (*MergeOutcome, error) {
	if method != "merge" && method != "squash" {
		return nil, fmt.Errorf("unsupported merge method %q (merge or squash)", method)
	}
	rep, err := g.Evaluate(ctx, cfg)
	if err != nil {
		return nil, err
	}
	out := &MergeOutcome{Report: rep}
	if !rep.Passed {
		out.Problem = "the gate did not pass; nothing was merged"
		return out, nil
	}
	cfg.defaults()
	// The last look before writing: the head, the draft flag and the base
	// branch, which are what can change between the evaluation and now.
	if why := g.lastLook(ctx, cfg); why != "" {
		out.Problem = "final re-check failed; nothing was merged: " + why
		return out, nil
	}
	out.Attempted = true
	res, merr := g.GH.MergePull(ctx, cfg.Owner, cfg.Repo, cfg.Number, cfg.HeadSHA, method)
	// Whatever the response, the pull request is read again: a lost
	// response may hide a merge, and a success is verified, not trusted.
	p, perr := g.GH.GetPull(ctx, cfg.Owner, cfg.Repo, cfg.Number)
	switch {
	case perr != nil:
		out.Problem = fmt.Sprintf("cannot re-read the pull request after the merge request (merge: %v): %v", merr, perr)
		if merr == nil && res != nil && res.Merged {
			out.Merged, out.MergeCommitSHA = true, res.SHA
		} else {
			out.Unknown = true
		}
		return out, nil
	case !p.Merged:
		if merr != nil {
			out.Problem = fmt.Sprintf("merge refused: %v", merr)
		} else {
			out.Problem = "GitHub answered the merge request but the pull request is not merged"
		}
		return out, nil
	}
	out.Merged, out.MergeCommitSHA = true, p.MergeCommitSHA
	if merr == nil && res != nil && res.SHA != p.MergeCommitSHA {
		out.Problem = fmt.Sprintf("merge response names %s but the pull request records %s", res.SHA, p.MergeCommitSHA)
		return out, nil
	}
	if !github.IsFullSHA(p.MergeCommitSHA) {
		out.Problem = fmt.Sprintf("merged, but the merge commit %q cannot be verified", p.MergeCommitSHA)
		return out, nil
	}
	cm, err := g.GH.GetCommit(ctx, cfg.Owner, cfg.Repo, p.MergeCommitSHA)
	if err != nil {
		out.Problem = fmt.Sprintf("merged, but the merge commit cannot be read: %v", err)
		return out, nil
	}
	var parents []string
	for _, pa := range cm.Parents {
		parents = append(parents, pa.SHA)
	}
	want := []string{cfg.BaseSHA, cfg.HeadSHA}
	if method == "squash" {
		want = want[:1]
	}
	if !slices.Equal(parents, want) {
		out.Problem = fmt.Sprintf("merged, but the merge commit's parents are %v, expected %v: the base moved between the gate and the merge; review %s on %s",
			parents, want, p.MergeCommitSHA, cfg.BaseRef)
		return out, nil
	}
	out.Verified = true
	return out, nil
}

func (g *Gate) lastLook(ctx context.Context, cfg Config) string {
	p, err := g.GH.GetPull(ctx, cfg.Owner, cfg.Repo, cfg.Number)
	switch {
	case err != nil:
		return fmt.Sprintf("cannot read the pull request: %v", err)
	case p.State != "open" || p.Merged || p.Draft:
		return fmt.Sprintf("state %q, merged %t, draft %t", p.State, p.Merged, p.Draft)
	case p.Head.SHA != cfg.HeadSHA:
		return fmt.Sprintf("head moved to %s", p.Head.SHA)
	case p.Base.Ref != cfg.BaseRef:
		return fmt.Sprintf("base changed to %q", p.Base.Ref)
	}
	ref, err := g.GH.GetRef(ctx, cfg.Owner, cfg.Repo, "heads/"+cfg.BaseRef)
	switch {
	case err != nil:
		return fmt.Sprintf("cannot read refs/heads/%s: %v", cfg.BaseRef, err)
	case ref.Object.SHA != cfg.BaseSHA:
		return fmt.Sprintf("%s moved to %s", cfg.BaseRef, ref.Object.SHA)
	}
	return ""
}
