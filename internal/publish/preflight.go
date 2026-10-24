package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/preflight"
)

// Deps are publish's connections to the outside world.
type Deps struct {
	Git Git
	GH  GitHub
	Out io.Writer
	// In answers the confirmation prompt; Interactive says whether it is a
	// terminal.
	In          io.Reader
	Interactive bool
	Now         func() time.Time
	Sleep       func(ctx context.Context, d time.Duration) error
	// Generic runs `hephaestus preflight --strict` for the tag (the
	// generic, read-only release checks, internal/preflight). It is an
	// additional gate: the checks above stay. Nil skips it (tests).
	Generic func(ctx context.Context, tag string) (*preflight.Report, error)
}

// Check is one preflight result.
type Check struct {
	Name   string
	OK     bool
	Detail string
	// Err marks a check that could not be made (API, network), as opposed
	// to one that found a problem.
	Err bool
}

// Report is the preflight outcome.
type Report struct {
	Checks []Check
	// Commit is the commit the tag will point to (local main == origin/main).
	Commit string
	// TapHeads records each tap's default branch head before the release,
	// keyed by owner/name, to prove later that a skipped tap was not changed.
	TapHeads map[string]string
}

func (r *Report) add(name string, ok bool, format string, args ...any) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
}

func (r *Report) addErr(name string, err error) {
	r.add(name, false, "%v", err)
	r.Checks[len(r.Checks)-1].Err = true
}

// Inconclusive reports whether every failed check failed only because it
// could not be made: nothing was found wrong yet.
func (r *Report) Inconclusive() bool {
	failed := false
	for _, c := range r.Checks {
		if !c.OK {
			if !c.Err {
				return false
			}
			failed = true
		}
	}
	return failed
}

// OK reports whether every check passed.
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return len(r.Checks) > 0
}

// Print writes the checks as a list.
func (r *Report) Print(w io.Writer) {
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %-28s %s\n", mark, c.Name, c.Detail)
	}
}

// Preflight checks, without writing anything, that tag pl.Version can be
// released now from the local main branch. The only local change is a
// fetch of origin/main.
func Preflight(ctx context.Context, d *Deps, p *Project, pl *Plan) *Report {
	r := &Report{TapHeads: map[string]string{}}
	tag := pl.Version.Tag
	r.add("version", true, "%s (%s)", tag, pl.Version.Kind())

	preflightGit(ctx, d, p, tag, r)
	preflightGitHub(ctx, d, p, pl, r)
	if pl.Version.Prerelease {
		preflightPrerelease(ctx, d, pl, r)
	} else {
		preflightStable(ctx, d, p, pl, r)
	}
	if d.Generic != nil {
		preflightGeneric(ctx, d, tag, r)
	}
	return r
}

// preflightGeneric adds the generic preflight (strict) as one more gate:
// its summary, and every check that did not pass.
func preflightGeneric(ctx context.Context, d *Deps, tag string, r *Report) {
	rep, err := d.Generic(ctx, tag)
	if err != nil {
		r.addErr("generic preflight", err)
		return
	}
	var parts []string
	for _, s := range []preflight.Status{preflight.Fail, preflight.Unknown, preflight.Warn, preflight.Pass, preflight.Skip} {
		if n := rep.Summary[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(s))))
		}
	}
	r.add("generic preflight", rep.ExitCode() == preflight.ExitPass, "hephaestus preflight --strict: %s (%s)", rep.Status, strings.Join(parts, ", "))
	for _, c := range rep.Checks {
		if c.Status == preflight.Fail || c.Status == preflight.Unknown || c.Status == preflight.Warn {
			r.add("  "+c.ID, false, "%s: %s", c.Status, c.Message)
		}
	}
}

func preflightGit(ctx context.Context, d *Deps, p *Project, tag string, r *Report) {
	top, err := d.Git.TopLevel(ctx)
	if err != nil {
		r.addErr("git repository", err)
		return
	}
	r.add("git repository", true, "%s", top)

	for _, push := range []bool{false, true} {
		name := "origin"
		if push {
			name = "origin push url"
		}
		url, err := d.Git.RemoteURL(ctx, "origin", push)
		if err != nil {
			r.addErr(name, err)
		} else if repo, err := ParseGitHubRemote(url); err != nil {
			r.addErr(name, err)
		} else if !strings.EqualFold(repo.String(), p.Repo.String()) {
			r.add(name, false, "%s is %s, the release repository is %s", name, repo, p.Repo)
		} else {
			r.add(name, true, "%s", p.Repo)
		}
	}

	branch, err := d.Git.CurrentBranch(ctx)
	switch {
	case err != nil:
		r.addErr("branch", err)
	case branch == "":
		r.add("branch", false, "HEAD is detached; switch to %s", p.Branch)
	case branch != p.Branch:
		r.add("branch", false, "on %s; releases are cut from %s", branch, p.Branch)
	default:
		r.add("branch", true, "%s", branch)
	}

	if st, err := d.Git.Status(ctx); err != nil {
		r.addErr("worktree", err)
	} else if st != "" {
		lines := strings.Split(st, "\n")
		r.add("worktree", false, "%d uncommitted or untracked paths (first: %s)", len(lines), strings.TrimSpace(lines[0]))
	} else {
		r.add("worktree", true, "clean")
	}

	if err := d.Git.FetchMain(ctx); err != nil {
		r.addErr("fetch origin/main", err)
		return
	}
	head, err1 := d.Git.Commit(ctx, "HEAD")
	remote, err2 := d.Git.Commit(ctx, "refs/remotes/origin/"+p.Branch)
	switch {
	case err1 != nil || err2 != nil:
		r.addErr("in sync with origin", errors.Join(err1, err2))
	case head == "" || remote == "":
		r.add("in sync with origin", false, "HEAD %q, origin/%s %q", head, p.Branch, remote)
	case head != remote:
		r.add("in sync with origin", false, "HEAD %s differs from origin/%s %s (unpushed or missing commits)", short(head), p.Branch, short(remote))
	default:
		r.Commit = head
		r.add("in sync with origin", true, "HEAD = origin/%s = %s", p.Branch, head)
	}

	if c, err := d.Git.Commit(ctx, "refs/tags/"+tag); err != nil {
		r.addErr("local tag", err)
	} else if c != "" {
		r.add("local tag", false, "%s already exists locally (-> %s); see make publish-status %s", tag, short(c), tag)
	} else {
		r.add("local tag", true, "%s does not exist", tag)
	}
	if c, ok, err := d.Git.RemoteTag(ctx, tag); err != nil {
		r.addErr("remote tag", err)
	} else if ok {
		r.add("remote tag", false, "%s already exists on origin (-> %s); see make publish-status %s", tag, short(c), tag)
	} else {
		r.add("remote tag", true, "%s does not exist on origin", tag)
	}
}

func preflightGitHub(ctx context.Context, d *Deps, p *Project, pl *Plan, r *Report) {
	if err := d.GH.AuthStatus(ctx); err != nil {
		r.addErr("gh auth", err)
		return
	}
	r.add("gh auth", true, "authenticated to github.com")

	if info, err := d.GH.Repo(ctx, p.Repo); err != nil {
		r.addErr("repository", err)
	} else if info.Archived || info.DefaultBranch != p.Branch {
		r.add("repository", false, "%s: archived %t, default branch %s", info.FullName, info.Archived, info.DefaultBranch)
	} else {
		r.add("repository", true, "%s, default branch %s", info.FullName, info.DefaultBranch)
	}

	if on, err := d.GH.ActionsEnabled(ctx, p.Repo); err != nil {
		r.addErr("actions", err)
	} else {
		r.add("actions", on, "GitHub Actions enabled: %t", on)
	}
	if w, err := d.GH.Workflow(ctx, p.Repo, p.ReleaseFile); err != nil {
		r.addErr("release workflow", err)
	} else {
		r.add("release workflow", w.State == "active" && w.Path == ".github/workflows/"+p.ReleaseFile, "%s (%s)", w.Path, w.State)
	}

	if r.Commit != "" {
		checkCI(ctx, d, p, r.Commit, r)
	}

	if sets, err := d.GH.Rulesets(ctx, p.Repo); err != nil {
		r.addErr("tag ruleset", err)
	} else {
		ok, detail := tagRulesetProtects(sets, pl.Version.Tag)
		r.add("tag ruleset", ok, "%s", detail)
		ok, detail = branchRequiresCheck(sets, p.Branch, p.RequiredCheck)
		r.add("main ruleset", ok, "%s", detail)
	}

	if env, err := d.GH.Environment(ctx, p.Repo, p.Environment); err != nil {
		r.addErr("environment", err)
	} else {
		ok, detail := environmentAllowsOnlyTag(env, pl.Version.Tag)
		r.add("environment", ok, "%s", detail)
	}

	if rels, err := d.GH.Releases(ctx, p.Repo); err != nil {
		r.addErr("no release yet", err)
	} else if i := slices.IndexFunc(rels, func(x Release) bool { return x.TagName == pl.Version.Tag }); i >= 0 {
		state := "published"
		if rels[i].Draft {
			state = "draft"
		}
		r.add("no release yet", false, "a %s release for %s already exists: %s", state, pl.Version.Tag, rels[i].HTMLURL)
	} else {
		r.add("no release yet", true, "no release or draft for %s", pl.Version.Tag)
	}

	r.add("release config", !pl.Draft, "release.github.draft: %t (a draft is never published)", pl.Draft)
}

// checkCI requires the CI workflow's push run on main for exactly commit to
// have succeeded. A green run of another commit does not count.
func checkCI(ctx context.Context, d *Deps, p *Project, commit string, r *Report) {
	runs, err := d.GH.Runs(ctx, p.Repo, p.CIFile, "push", p.Branch, commit)
	if err != nil {
		r.addErr("ci on commit", err)
		return
	}
	var mine []Run
	for _, run := range runs {
		if run.HeadSHA == commit && run.Event == "push" && run.HeadBranch == p.Branch && run.Path == ".github/workflows/"+p.CIFile {
			mine = append(mine, run)
		}
	}
	if len(mine) == 0 {
		r.add("ci on commit", false, "no %s push run on %s for %s", p.CIFile, p.Branch, short(commit))
		return
	}
	latest := mine[0]
	for _, run := range mine[1:] {
		if run.CreatedAt.After(latest.CreatedAt) {
			latest = run
		}
	}
	switch {
	case latest.Status != "completed":
		r.add("ci on commit", false, "%s is %s: wait for it (%s)", p.CIFile, latest.Status, latest.HTMLURL)
	case latest.Conclusion != "success":
		r.add("ci on commit", false, "%s concluded %s (%s)", p.CIFile, latest.Conclusion, latest.HTMLURL)
	default:
		r.add("ci on commit", true, "%s succeeded on %s (%s)", p.CIFile, short(commit), latest.HTMLURL)
	}
}

func preflightPrerelease(ctx context.Context, d *Deps, pl *Plan, r *Report) {
	r.add("prerelease", pl.Prerelease, "the GitHub release is marked prerelease: %t (never \"latest\")", pl.Prerelease)
	for _, t := range pl.Taps {
		r.add("tap skipped", t.Publish.Skip, "%s: %s", t.Formula, orDefault(t.Publish.SkipReason, "would be published for a prerelease"))
		recordTapHead(ctx, d, t, r)
	}
}

func preflightStable(ctx context.Context, d *Deps, p *Project, pl *Plan, r *Report) {
	r.add("not a prerelease", !pl.Prerelease, "release.github.prerelease marks %s prerelease: %t", pl.Version.Tag, pl.Prerelease)
	if len(pl.Taps) == 0 {
		return
	}
	if names, err := d.GH.EnvironmentSecrets(ctx, p.Repo, p.Environment); err != nil {
		r.addErr("tap token secret", err)
	} else {
		r.add("tap token secret", slices.Contains(names, p.TapTokenSecret), "%s in environment %s (name only)", p.TapTokenSecret, p.Environment)
	}
	for _, t := range pl.Taps {
		pub := t.Publish
		if pub.Skip {
			r.add("tap publish", false, "%s: %s; a stable release is expected to update the tap", t.Formula, pub.SkipReason)
			continue
		}
		tap := Repo{Owner: pub.Owner, Name: pub.Name}
		r.add("tap mode", pub.PullRequest, "%s: %s", t.Formula, describeMode(pub))
		info, err := d.GH.Repo(ctx, tap)
		if err != nil {
			r.addErr("tap repository", err)
			continue
		}
		r.add("tap repository", !info.Archived, "%s (default branch %s)", info.FullName, info.DefaultBranch)
		recordTapHead(ctx, d, t, r)

		entries, err := d.GH.Contents(ctx, tap, "", info.DefaultBranch)
		if err != nil {
			r.addErr("tap layout", err)
		} else {
			ok, detail := formulaDirMatches(entries, pub.Path)
			r.add("tap layout", ok, "%s", detail)
		}

		if old, err := d.GH.File(ctx, tap, pub.Path, info.DefaultBranch); err == nil {
			r.add("tap formula", !strings.Contains(string(old), "/"+pl.Version.Tag+"/"), "%s exists and will be updated", pub.Path)
		} else if errors.Is(err, ErrNotFound) {
			r.add("tap formula", true, "%s will be added", pub.Path)
		} else {
			r.addErr("tap formula", err)
		}

		if pub.PullRequest {
			prs, err := d.GH.PullRequests(ctx, tap, pub.Owner, pub.Branch)
			if err != nil {
				r.addErr("tap pull request", err)
			} else if len(prs) > 0 {
				r.add("tap pull request", false, "a pull request from %s already exists: %s", pub.Branch, prs[0].HTMLURL)
			} else if _, err := d.GH.BranchHead(ctx, tap, pub.Branch); err == nil {
				r.add("tap pull request", false, "branch %s already exists on %s", pub.Branch, tap)
			} else if !errors.Is(err, ErrNotFound) {
				r.addErr("tap pull request", err)
			} else {
				r.add("tap pull request", true, "will open from %s:%s", pub.Owner, pub.Branch)
			}
		}

		if t.URLPrefix != "" {
			r.add("formula urls", true, "%s<archive>", t.URLPrefix)
		} else {
			r.add("formula urls", false, "brews url_template is set: publish verifies release download URLs only")
		}
	}
	r.add("tap token scope", true, "checked by the release workflow's read-only tap rehearsal before the release is published")
}

func recordTapHead(ctx context.Context, d *Deps, t TapPlan, r *Report) {
	if t.Publish.Owner == "" || t.Publish.Name == "" {
		return
	}
	tap := Repo{Owner: t.Publish.Owner, Name: t.Publish.Name}
	info, err := d.GH.Repo(ctx, tap)
	if err != nil {
		r.addErr("tap head", err)
		return
	}
	head, err := d.GH.BranchHead(ctx, tap, info.DefaultBranch)
	if err != nil {
		r.addErr("tap head", err)
		return
	}
	r.TapHeads[tap.String()] = head
	r.add("tap head", true, "%s@%s is %s", tap, info.DefaultBranch, short(head))
}

func describeMode(p homebrew.Publish) string {
	if p.PullRequest {
		return fmt.Sprintf("pull request from %s:%s into %s/%s", p.Owner, p.Branch, p.Owner, p.Name)
	}
	return fmt.Sprintf("direct commit to %s/%s; publish expects a pull request", p.Owner, p.Name)
}

// tagRulesetProtects requires an active tag ruleset covering tag that
// forbids deletion and update, and no active rule that blocks creating it.
func tagRulesetProtects(sets []Ruleset, tag string) (bool, string) {
	ref := "refs/tags/" + tag
	var protecting []string
	for _, rs := range sets {
		if rs.Target != "tag" || !refIncluded(rs, ref, "") {
			continue
		}
		types := ruleTypes(rs)
		if rs.Enforcement == "active" && slices.Contains(types, "creation") {
			return false, fmt.Sprintf("ruleset %q blocks creating %s", rs.Name, tag)
		}
		if rs.Enforcement == "active" && slices.Contains(types, "deletion") && slices.Contains(types, "update") {
			name := rs.Name
			if n := len(rs.BypassActors); n > 0 {
				name += fmt.Sprintf(" (warning: %d bypass actors can still move or delete it)", n)
			}
			protecting = append(protecting, name)
		}
	}
	if len(protecting) == 0 {
		return false, fmt.Sprintf("no active tag ruleset forbids deleting and updating %s", ref)
	}
	return true, fmt.Sprintf("%s: deletion and update forbidden, creation allowed", strings.Join(protecting, ", "))
}

// branchRequiresCheck requires an active branch ruleset on branch that
// requires check.
func branchRequiresCheck(sets []Ruleset, branch, check string) (bool, string) {
	ref := "refs/heads/" + branch
	for _, rs := range sets {
		if rs.Target != "branch" || rs.Enforcement != "active" || !refIncluded(rs, ref, branch) {
			continue
		}
		for _, rule := range rs.Rules {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, c := range rule.Parameters.RequiredStatusChecks {
				if c.Context == check {
					return true, fmt.Sprintf("%s requires %s on %s", rs.Name, check, branch)
				}
			}
		}
	}
	return false, fmt.Sprintf("no active ruleset requires %s on %s", check, branch)
}

func ruleTypes(rs Ruleset) []string {
	var out []string
	for _, r := range rs.Rules {
		out = append(out, r.Type)
	}
	return out
}

// refIncluded evaluates a ruleset's ref_name condition for ref. Patterns
// are fnmatch-style: "*" does not cross "/". defaultBranch, when set, is
// the branch ~DEFAULT_BRANCH stands for.
func refIncluded(rs Ruleset, ref, defaultBranch string) bool {
	match := func(patterns []string) bool {
		for _, pat := range patterns {
			switch pat {
			case "~ALL":
				return true
			case "~DEFAULT_BRANCH":
				if defaultBranch != "" && ref == "refs/heads/"+defaultBranch {
					return true
				}
				continue
			}
			if ok, _ := path.Match(pat, ref); ok {
				return true
			}
		}
		return false
	}
	c := rs.Conditions.RefName
	return match(c.Include) && !match(c.Exclude)
}

// environmentAllowsOnlyTag requires deployments to be limited to tag
// policies, one of which matches tag.
func environmentAllowsOnlyTag(env Environment, tag string) (bool, string) {
	if !env.CustomPolicies {
		if env.Protected {
			return false, fmt.Sprintf("%s deploys from protected branches; want only v* tags", env.Name)
		}
		return false, fmt.Sprintf("%s has no deployment policy: any branch or tag can deploy", env.Name)
	}
	matched := false
	var names []string
	for _, pol := range env.Policies {
		names = append(names, pol.Type+":"+pol.Name)
		if pol.Type != "tag" {
			return false, fmt.Sprintf("%s also deploys from %s %q; want tags only", env.Name, pol.Type, pol.Name)
		}
		if ok, _ := path.Match(pol.Name, tag); ok {
			matched = true
		}
	}
	if !matched {
		return false, fmt.Sprintf("%s policies %v do not allow %s", env.Name, names, tag)
	}
	return true, fmt.Sprintf("%s deploys only from %s", env.Name, strings.Join(names, ", "))
}

// formulaDirMatches checks that Homebrew will read the formula where the
// tap stage writes it. Homebrew uses Formula/, else HomebrewFormula/, else
// the repository root.
func formulaDirMatches(entries []Content, formulaPath string) (bool, string) {
	dir := path.Dir(formulaPath)
	homebrewDir := "."
	for _, cand := range []string{"Formula", "HomebrewFormula"} {
		if slices.ContainsFunc(entries, func(c Content) bool { return c.Type == "dir" && c.Name == cand }) {
			homebrewDir = cand
			break
		}
	}
	if dir != homebrewDir {
		return false, fmt.Sprintf("the formula goes to %s but Homebrew reads formulae from %s/", formulaPath, homebrewDir)
	}
	return true, fmt.Sprintf("%s is where Homebrew reads formulae (%s/)", formulaPath, homebrewDir)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
