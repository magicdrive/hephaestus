package preflight

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/archive"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/tap"
	"github.com/magicdrive/hephaestus/internal/tmpl"
	"github.com/magicdrive/hephaestus/internal/version"
)

// registry is every check, in execution order. IDs are stable: they are
// part of the JSON report and of --skip.
var registry = []def{
	{id: "git.repository", group: "git", title: "Inside a git work tree", severity: SeverityError, run: checkGitRepository},
	{id: "git.remote", group: "git", title: "Remote is the release repository", severity: SeverityError, needs: []string{"git.repository"}, run: checkGitRemote},
	{id: "git.worktree", group: "git", title: "Work tree is clean", severity: SeverityError, needs: []string{"git.repository"}, run: checkGitWorktree},
	{id: "git.branch", group: "git", title: "On the release branch", severity: SeverityError, needs: []string{"git.repository"}, run: checkGitBranch},
	{id: "git.head.pushed", group: "git", title: "HEAD is the remote release branch", severity: SeverityError, needs: []string{"git.repository"}, network: true, run: checkGitHeadPushed},
	{id: "tag.format", group: "tag", title: "Tag is a valid release version", severity: SeverityError, run: checkTagFormat},
	{id: "tag.local", group: "tag", title: "Tag does not exist locally", severity: SeverityError, needs: []string{"git.repository", "tag.format"}, run: checkTagLocal},
	{id: "tag.remote", group: "tag", title: "Tag does not exist on the remote", severity: SeverityError, needs: []string{"git.repository", "tag.format"}, network: true, run: checkTagRemote},
	{id: "tag.order", group: "tag", title: "Tag is newer than the released versions", severity: SeverityWarning, needs: []string{"git.repository", "tag.format"}, network: true, run: checkTagOrder},
	{id: "config.load", group: "config", title: "Configuration is valid", severity: SeverityError, run: checkConfigLoad},
	{id: "config.assets", group: "config", title: "Release asset names are valid and unique", severity: SeverityError, needs: []string{"config.load", "tag.format"}, run: checkConfigAssets},
	{id: "config.release", group: "config", title: "GitHub release settings", severity: SeverityError, needs: []string{"config.load", "tag.format"}, run: checkConfigRelease},
	{id: "config.formula", group: "config", title: "Homebrew formula publication resolves", severity: SeverityError, needs: []string{"config.load", "tag.format"}, run: checkConfigFormula},
	{id: "github.repository", group: "github", title: "Release repository is reachable", severity: SeverityError, network: true, run: checkGitHubRepository},
	{id: "github.release", group: "github", title: "No release or draft exists for the tag", severity: SeverityError, needs: []string{"github.repository", "config.release"}, network: true, run: checkGitHubRelease},
	{id: "github.ci", group: "github", title: "CI succeeded on the exact commit", severity: SeverityError, needs: []string{"github.repository", "git.repository"}, network: true, run: checkGitHubCI},
	{id: "github.workflow", group: "github", title: "Release workflow is active", severity: SeverityError, needs: []string{"github.repository"}, network: true, run: checkGitHubWorkflow},
	{id: "github.environment", group: "github", title: "Environment deploys only from release tags", severity: SeverityError, needs: []string{"github.repository", "tag.format"}, network: true, run: checkGitHubEnvironment},
	{id: "github.secrets", group: "github", title: "Required secrets exist (names only)", severity: SeverityError, needs: []string{"github.repository"}, network: true, run: checkGitHubSecrets},
	{id: "github.ruleset.branch", group: "github", title: "Release branch is protected by a ruleset", severity: SeverityWarning, needs: []string{"github.repository"}, network: true, run: checkRulesetBranch},
	{id: "github.ruleset.tag", group: "github", title: "Release tags are protected by a ruleset", severity: SeverityError, needs: []string{"github.repository", "tag.format"}, network: true, run: checkRulesetTag},
	{id: "tap.repository", group: "tap", title: "Tap repository is reachable", severity: SeverityError, needs: []string{"config.formula", "github.repository"}, network: true, run: checkTapRepository},
	{id: "tap.layout", group: "tap", title: "Formula goes where Homebrew reads it", severity: SeverityError, needs: []string{"tap.repository"}, network: true, run: checkTapLayout},
	{id: "tap.branch", group: "tap", title: "Formula pull request branch is free", severity: SeverityError, needs: []string{"tap.repository"}, network: true, run: checkTapBranch},
	{id: "tap.pull-request", group: "tap", title: "No pull request exists for the formula branch", severity: SeverityError, needs: []string{"tap.repository"}, network: true, run: checkTapPullRequest},
}

// --- git ---

func checkGitRepository(s *state) outcome {
	top, err := s.in.Git.TopLevel(s.ctx)
	if err != nil {
		return fail("not inside a git work tree", "run hephaestus preflight in the repository you release", ev{"error": err.Error()})
	}
	head, err := s.in.Git.Head(s.ctx)
	if err != nil || head == "" {
		return fail("HEAD has no commit", "commit before releasing", ev{"top_level": top})
	}
	s.head = head
	return pass(top, ev{"top_level": top, "head": head})
}

// redactURL removes credentials from a remote URL for display.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return raw
}

func checkGitRemote(s *state) outcome {
	r := s.settings.remote
	e := ev{"remote": r}
	var mismatch []string
	for _, push := range []bool{false, true} {
		kind := "fetch"
		if push {
			kind = "push"
		}
		u, err := s.in.Git.RemoteURL(s.ctx, r, push)
		if err != nil {
			return fail(fmt.Sprintf("remote %q is not configured", r), "add the remote or set preflight.remote / --remote", e)
		}
		e[kind+"_url"] = redactURL(u)
		_, owner, name, err := github.ParseRemoteURL(u)
		if err != nil {
			return fail(fmt.Sprintf("%s URL of %q: %v", kind, r, err), "point the remote at the GitHub repository", e)
		}
		if s.owner != "" && !strings.EqualFold(owner+"/"+name, s.repoFullName()) {
			mismatch = append(mismatch, fmt.Sprintf("%s URL is %s/%s", kind, owner, name))
		}
	}
	e["release_repository"] = s.repoFullName()
	e["release_repository_source"] = s.repoSource
	if len(mismatch) > 0 {
		return fail(fmt.Sprintf("%s, but releases go to %s (%s)", strings.Join(mismatch, " and "), s.repoFullName(), s.repoSource),
			"release from a clone of the release repository, or fix release.github / --repo", e)
	}
	if s.owner == "" {
		return fail("the release repository is unknown", "set release.github.owner/name or --repo", e)
	}
	return pass(fmt.Sprintf("%s → %s", r, s.repoFullName()), e)
}

func checkGitWorktree(s *state) outcome {
	st, err := s.in.Git.Status(s.ctx)
	if err != nil {
		return unknownf("git status: %v", err)
	}
	if st == "" {
		return pass("clean", nil)
	}
	lines := strings.Split(st, "\n")
	shown := lines
	if len(shown) > 10 {
		shown = shown[:10]
	}
	return fail(fmt.Sprintf("%s uncommitted or untracked", plural(len(lines), "path is", "paths are")),
		"commit, stash or remove them: a release is built from the commit, not the work tree", ev{"paths": shown})
}

func checkGitBranch(s *state) outcome {
	cur, err := s.in.Git.Branch(s.ctx)
	if err != nil {
		return unknownf("current branch: %v", err)
	}
	s.branch = cur
	want, src, err := s.defaultBranch()
	e := ev{"current": cur, "release_branch": want, "release_branch_source": src}
	switch {
	case cur == "":
		return fail("HEAD is detached", "check out the release branch", e)
	case err != nil:
		o := unknownf("release branch %v", err)
		o.evidence = e
		return o
	case cur != want:
		return fail(fmt.Sprintf("on %s, releases are tagged on %s", cur, want), "check out "+want, e)
	}
	return pass(cur, e)
}

func checkGitHeadPushed(s *state) outcome {
	b, _, err := s.defaultBranch()
	if err != nil {
		return unknownf("release branch %v", err)
	}
	refs, err := s.getRemoteRefs()
	if err != nil {
		return unknownf("git ls-remote %s: %v", s.settings.remote, err)
	}
	remote := refs["refs/heads/"+b]
	e := ev{"head": s.head, "remote_branch": s.settings.remote + "/" + b, "remote_sha": remote}
	switch {
	case remote == "":
		return fail(fmt.Sprintf("%s has no branch %s", s.settings.remote, b), "push the release branch", e)
	case remote != s.head:
		return fail(fmt.Sprintf("HEAD %s differs from %s/%s %s", shortSHA(s.head), s.settings.remote, b, shortSHA(remote)),
			"pull or push so that HEAD is exactly the remote release branch (the commit CI tested)", e)
	}
	return pass(fmt.Sprintf("HEAD = %s/%s = %s", s.settings.remote, b, shortSHA(s.head)), e)
}

// --- tag ---

func checkTagFormat(s *state) outcome {
	info, err := version.Parse(s.in.Tag)
	if err != nil {
		return fail(err.Error(), "use a tag like v1.2.3 or v1.2.3-rc.1", ev{"tag": s.in.Tag})
	}
	s.info, s.tagOK, s.prerelease = info, true, info.IsPrerelease()
	e := ev{"tag": info.Tag, "version": info.Version, "semver": info.IsSemver(), "prerelease": info.IsPrerelease()}
	if !info.IsSemver() {
		return warn(fmt.Sprintf("%s is not SemVer: prerelease detection and ordering do not apply", info.Tag),
			"use vMAJOR.MINOR.PATCH[-PRERELEASE]", e)
	}
	kind := "stable"
	if info.IsPrerelease() {
		kind = "prerelease"
	}
	return pass(fmt.Sprintf("%s (%s)", info.Tag, kind), e)
}

func checkTagLocal(s *state) outcome {
	c, err := s.in.Git.LocalTag(s.ctx, s.in.Tag)
	if err != nil {
		return unknownf("local tag: %v", err)
	}
	if c != "" {
		return fail(fmt.Sprintf("%s already exists locally (→ %s)", s.in.Tag, shortSHA(c)),
			"a pushed tag is final; if it was never pushed, inspect and delete it (git tag -d), otherwise release a new version", ev{"commit": c})
	}
	return pass("absent", nil)
}

func checkTagRemote(s *state) outcome {
	refs, err := s.getRemoteRefs()
	if err != nil {
		return unknownf("git ls-remote %s: %v", s.settings.remote, err)
	}
	ref := "refs/tags/" + s.in.Tag
	if c, ok := refs[ref+"^{}"]; ok {
		return fail(fmt.Sprintf("%s already exists on %s (→ %s)", s.in.Tag, s.settings.remote, shortSHA(c)), "release a new version", ev{"commit": c})
	}
	if c, ok := refs[ref]; ok {
		return fail(fmt.Sprintf("%s already exists on %s (→ %s)", s.in.Tag, s.settings.remote, shortSHA(c)), "release a new version", ev{"object": c})
	}
	return pass("absent on "+s.settings.remote, nil)
}

func checkTagOrder(s *state) outcome {
	if !s.info.IsSemver() {
		return skip("the tag is not SemVer")
	}
	refs, err := s.getRemoteRefs()
	if err != nil {
		return unknownf("git ls-remote %s: %v", s.settings.remote, err)
	}
	var best version.Info
	for ref := range refs {
		name, ok := strings.CutPrefix(ref, "refs/tags/")
		if !ok || strings.HasSuffix(name, "^{}") {
			continue
		}
		i, err := version.Parse(name)
		if err != nil || !i.IsSemver() || i.IsPrerelease() {
			continue
		}
		if best.Tag == "" || compareCore(i, best) > 0 {
			best = i
		}
	}
	if best.Tag == "" {
		return pass("no stable release tag yet", nil)
	}
	e := ev{"latest_stable": best.Tag}
	if compareCore(s.info, best) <= 0 {
		return warn(fmt.Sprintf("%s is not newer than the released %s", s.in.Tag, best.Tag), "release a higher version", e)
	}
	return pass(fmt.Sprintf("newer than %s", best.Tag), e)
}

// compareCore compares MAJOR.MINOR.PATCH numerically.
func compareCore(a, b version.Info) int {
	for _, p := range [][2]string{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if len(p[0]) != len(p[1]) {
			return len(p[0]) - len(p[1])
		}
		if c := strings.Compare(p[0], p[1]); c != 0 {
			return c
		}
	}
	return 0
}

// --- config ---

func checkConfigLoad(s *state) outcome {
	if s.in.ConfigErr != nil {
		return fail(s.in.ConfigErr.Error(), "fix the configuration (hephaestus check)", ev{"path": s.in.ConfigPath})
	}
	if s.in.Config == nil {
		return fail("no configuration", "add hephaestus.yaml or pass --config", nil)
	}
	return pass(s.in.ConfigPath, ev{"path": s.in.ConfigPath, "project": s.in.Config.Project.Name})
}

// vars are the template variables of the tag, as the pipeline sets them
// (the environment without credentials).
func (s *state) vars() tmpl.Vars {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && !tmpl.IsCredentialName(k) {
			env[k] = v
		}
	}
	for _, e := range s.in.Config.Env {
		k, v, _ := strings.Cut(string(e), "=")
		env[k] = v
	}
	n := s.in.Config.Project.Name
	return tmpl.Vars{Project: n, ProjectName: n, Version: s.info.Version, Tag: s.info.Tag, RawVersion: s.info.Tag,
		Major: s.info.Major, Minor: s.info.Minor, Patch: s.info.Patch, Prerelease: s.info.Prerelease, Commit: s.head, Env: env}
}

func checkConfigAssets(s *state) outcome {
	cfg := s.in.Config
	expected, unknown, err := archive.ExpectedArchives(cfg, s.vars())
	if err != nil {
		return fail(err.Error(), "fix the archive or build name templates", nil)
	}
	var names []string
	for _, e := range expected {
		names = append(names, e.Name)
	}
	if !cfg.Checksum.Disable && len(cfg.Archives)+len(expected) > 0 {
		cs, err := tmpl.Render(cfg.Checksum.Name, s.vars())
		if err != nil {
			return fail("checksum.name: "+err.Error(), "fix checksum.name", nil)
		}
		names = append(names, cs)
	}
	if len(names) == 0 && len(unknown) == 0 {
		return skip("the configuration produces no release assets")
	}
	e := ev{"assets": names}
	var problems []string
	for _, n := range names {
		if err := archive.AssetNameProblem(n); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, g := range archive.FoldCollisions(names) {
		problems = append(problems, "names collide (GitHub compares them case-insensitively): "+strings.Join(g, ", "))
	}
	if len(problems) > 0 {
		return fail(strings.Join(problems, "; "), "make every asset name unique, e.g. include {{ .OS }} and {{ .Arch }}", e)
	}
	if len(unknown) > 0 {
		e["unknown"] = unknown
		return warn(fmt.Sprintf("%s planned; %s", plural(len(names), "asset", "assets"), strings.Join(unknown, "; ")),
			"those names are checked when the archive stage runs", e)
	}
	return pass(plural(len(names), "asset", "assets")+" with unique names", e)
}

func checkConfigRelease(s *state) outcome {
	g := s.in.Config.Release.GitHub
	if !g.IsEnabled() {
		return skip("no GitHub release configured")
	}
	pre := s.prerelease
	switch g.Prerelease {
	case "true":
		pre = true
	case "false":
		pre = false
	}
	e := ev{"repository": s.repoFullName(), "prerelease": pre, "draft": g.Draft}
	if g.MakeLatest != "" {
		e["make_latest"] = g.MakeLatest
	}
	if g.Draft {
		return warn("release.github.draft is true: the release is left as a draft and never published", "set draft: false to publish", e)
	}
	kind := "published as a stable release"
	if pre {
		kind = "published as a prerelease (not latest)"
	}
	return pass(kind, e)
}

// tapPlan is one formula published to a tap for this tag.
type tapPlan struct {
	formula, owner, name, branch, path string
	pullRequest                        bool
	baseOwner, baseName, baseBranch    string
}

func checkConfigFormula(s *state) outcome {
	cfg := s.in.Config
	if len(cfg.Brews) == 0 {
		return skip("no Homebrew formulae configured")
	}
	var lines []string
	var problems []string
	for i := range cfg.Brews {
		b := &cfg.Brews[i]
		pub, err := homebrew.ResolvePublish(b, s.vars())
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", b.Name, err))
			continue
		}
		switch {
		case pub.Skip:
			lines = append(lines, fmt.Sprintf("%s: tap skipped (%s)", b.Name, pub.SkipReason))
		case pub.Owner == "" || pub.Name == "":
			lines = append(lines, fmt.Sprintf("%s: formula generated, no tap configured", b.Name))
		default:
			mode := "direct commit"
			if pub.PullRequest {
				mode = "pull request from " + pub.Branch
			}
			lines = append(lines, fmt.Sprintf("%s → %s/%s:%s (%s)", b.Name, pub.Owner, pub.Name, pub.Path, mode))
			s.taps = append(s.taps, tapPlan{formula: b.Name, owner: pub.Owner, name: pub.Name, branch: pub.Branch, path: pub.Path,
				pullRequest: pub.PullRequest, baseOwner: pub.BaseOwner, baseName: pub.BaseName, baseBranch: pub.BaseBranch})
		}
	}
	e := ev{"formulae": lines}
	if len(problems) > 0 {
		return fail(strings.Join(problems, "; "), "fix brews[] publication settings", e)
	}
	return pass(strings.Join(lines, "; "), e)
}

// --- github ---

func checkGitHubRepository(s *state) outcome {
	if s.owner == "" {
		return fail("the release repository is unknown", "set release.github.owner/name, --repo, or a GitHub remote", nil)
	}
	if s.in.GitHub == nil {
		return skip("no GitHub client")
	}
	r, err := s.getRepoInfo()
	if isNotFound(err) {
		return fail(s.repoFullName()+" was not found (or the token cannot see it)", "check the repository name and the token", nil)
	}
	if err != nil {
		return apiOutcome(s.repoFullName(), err)
	}
	e := ev{"repository": r.FullName, "default_branch": r.DefaultBranch, "private": r.Private, "token": s.in.TokenPresent}
	if r.Archived {
		return fail(r.FullName+" is archived", "unarchive it before releasing", e)
	}
	return pass(fmt.Sprintf("%s (default branch %s)", r.FullName, r.DefaultBranch), e)
}

func checkGitHubRelease(s *state) outcome {
	rels, err := s.getReleases()
	if err != nil {
		return apiOutcome("releases", err)
	}
	for _, r := range rels {
		if r.TagName != s.in.Tag {
			continue
		}
		kind := "published"
		if r.Draft {
			kind = "draft"
		}
		return fail(fmt.Sprintf("a %s release for %s already exists", kind, s.in.Tag),
			"a published release is final: release a new version; a leftover draft must be inspected and deleted first",
			ev{"url": r.HTMLURL, "draft": r.Draft, "immutable": r.Immutable})
	}
	if !s.in.TokenPresent {
		return warn("no release for "+s.in.Tag+" is visible, but drafts are only listed for a token with push access",
			"run with a token (GITHUB_TOKEN) to see drafts", ev{"releases_seen": len(rels)})
	}
	return pass("no release or draft for "+s.in.Tag, ev{"releases_seen": len(rels)})
}

var failedConclusions = []string{"failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale"}

func checkGitHubCI(s *state) outcome {
	runs, err := s.in.GitHub.ListCheckRuns(s.ctx, s.owner, s.repo, s.head)
	if err != nil {
		return apiOutcome("check runs of "+shortSHA(s.head), err)
	}
	status, err := s.in.GitHub.GetCombinedStatus(s.ctx, s.owner, s.repo, s.head)
	if err != nil {
		return apiOutcome("commit status of "+shortSHA(s.head), err)
	}
	type result struct{ state, url string }
	got := map[string]result{} // latest result per check name
	var failed, pending []string
	for _, r := range runs {
		st := r.Conclusion
		if r.Status != "completed" {
			st = "pending"
		}
		got[r.Name] = result{st, r.HTMLURL}
		switch {
		case st == "pending":
			pending = append(pending, r.Name)
		case slices.Contains(failedConclusions, st):
			failed = append(failed, r.Name+" ("+st+")")
		}
	}
	for _, st := range status.Statuses {
		got[st.Context] = result{state: st.State}
		switch st.State {
		case "pending":
			pending = append(pending, st.Context)
		case "failure", "error":
			failed = append(failed, st.Context+" ("+st.State+")")
		}
	}
	var required []string
	if sets, err := s.getRulesets(); err == nil {
		if b, _, err := s.defaultBranch(); err == nil {
			for _, rs := range activeCovering(sets, "branch", "refs/heads/"+b, b) {
				for _, rule := range rs.Rules {
					for _, c := range rule.Parameters.RequiredStatusChecks {
						if !slices.Contains(required, c.Context) {
							required = append(required, c.Context)
						}
					}
				}
			}
		}
	}
	var missing []string
	for _, c := range required {
		if r, ok := got[c]; !ok || r.state != "success" {
			missing = append(missing, c)
		}
	}
	sort.Strings(failed)
	sort.Strings(pending)
	e := ev{"commit": s.head, "check_runs": len(runs), "statuses": status.TotalCount, "required": required}
	switch {
	case len(failed) > 0:
		e["failed"] = failed
		return fail(fmt.Sprintf("%s failed on %s", strings.Join(failed, ", "), shortSHA(s.head)), "fix CI on the release branch before tagging", e)
	case len(pending) > 0:
		e["pending"] = pending
		return fail(fmt.Sprintf("%s still running on %s", strings.Join(pending, ", "), shortSHA(s.head)), "wait for CI to finish", e)
	case len(missing) > 0:
		e["missing"] = missing
		return fail(fmt.Sprintf("required checks %s did not succeed on %s", strings.Join(missing, ", "), shortSHA(s.head)),
			"push the commit through the normal CI and wait for the required checks", e)
	case len(runs)+status.TotalCount == 0:
		return warn("no CI results on "+shortSHA(s.head), "run CI on the commit before tagging", e)
	}
	return pass(fmt.Sprintf("%s succeeded on %s", plural(len(got), "check", "checks"), shortSHA(s.head)), e)
}

func checkGitHubWorkflow(s *state) outcome {
	f := s.settings.workflow
	if f == "" {
		return skip("no release workflow configured (preflight.workflow / --workflow)")
	}
	w, err := s.in.GitHub.GetWorkflow(s.ctx, s.owner, s.repo, f)
	if isNotFound(err) {
		return fail(f+" is not a workflow of "+s.repoFullName()+" on the default branch", "commit .github/workflows/"+f+" to the default branch", nil)
	}
	if err != nil {
		return apiOutcome("workflow "+f, err)
	}
	e := ev{"path": w.Path, "state": w.State}
	if w.State != "active" {
		return fail(fmt.Sprintf("%s is %s", w.Path, w.State), "enable the workflow", e)
	}
	return pass(w.Path+" active", e)
}

func checkGitHubEnvironment(s *state) outcome {
	name := s.settings.environment
	if name == "" {
		return skip("no environment configured (preflight.environment / --environment)")
	}
	env, err := s.in.GitHub.GetEnvironment(s.ctx, s.owner, s.repo, name)
	if isNotFound(err) {
		return fail("environment "+name+" does not exist", "create it with a tag deployment policy (docs/preflight.md#recommended-settings)", nil)
	}
	if err != nil {
		return apiOutcome("environment "+name, err)
	}
	p := env.DeploymentBranchPolicy
	switch {
	case p == nil:
		return fail(name+" has no deployment policy: any branch or tag can deploy and read its secrets",
			"restrict deployments to tags matching the release tags (e.g. v*)", ev{"environment": name})
	case p.ProtectedBranches:
		return fail(name+" deploys from protected branches, not from release tags", "use custom policies with a tag rule (e.g. v*)", ev{"environment": name})
	}
	pols, err := s.in.GitHub.ListDeploymentPolicies(s.ctx, s.owner, s.repo, name)
	if err != nil {
		return apiOutcome("deployment policies of "+name, err)
	}
	var shown, branches []string
	matched := false
	for _, pol := range pols {
		shown = append(shown, pol.Type+":"+pol.Name)
		if pol.Type != "tag" {
			branches = append(branches, pol.Name)
			continue
		}
		if ok, _ := path.Match(pol.Name, s.in.Tag); ok {
			matched = true
		}
	}
	e := ev{"environment": name, "policies": shown}
	switch {
	case len(branches) > 0:
		return fail(fmt.Sprintf("%s also deploys from branches %s: a branch with that name could read the release secrets", name, strings.Join(branches, ", ")),
			"remove the branch policies; keep only tag policies", e)
	case !matched:
		return fail(fmt.Sprintf("no tag policy of %s matches %s: the publishing job would be refused", name, s.in.Tag), "add a tag policy matching the release tags", e)
	}
	return pass(fmt.Sprintf("%s deploys only from tags (%s)", name, strings.Join(shown, ", ")), e)
}

func checkGitHubSecrets(s *state) outcome {
	required := slices.Clone(s.settings.secrets)
	if len(s.taps) > 0 && !slices.Contains(required, s.settings.tapTokenSecret) && !s.tapTokenFromConfig() {
		required = append(required, s.settings.tapTokenSecret)
	}
	if len(required) == 0 {
		why := "no secret is required for this tag"
		if s.prerelease && s.results["config.formula"].Status == Pass {
			why += " (the tap is skipped for prereleases, so its token is not needed)"
		}
		return skip(why)
	}
	have := map[string]string{}
	if env := s.settings.environment; env != "" {
		names, err := s.in.GitHub.ListEnvironmentSecretNames(s.ctx, s.owner, s.repo, env)
		if err != nil {
			return apiOutcome("secret names of environment "+env, err)
		}
		for _, n := range names {
			have[n] = "environment " + env
		}
	}
	names, err := s.in.GitHub.ListRepositorySecretNames(s.ctx, s.owner, s.repo)
	if err != nil {
		return apiOutcome("repository secret names", err)
	}
	for _, n := range names {
		if _, ok := have[n]; !ok {
			have[n] = "repository"
		}
	}
	found := ev{}
	var missing []string
	for _, n := range required {
		if where, ok := have[n]; ok {
			found[n] = where
		} else {
			missing = append(missing, n)
		}
	}
	e := ev{"required": required, "found_in": found}
	if len(missing) > 0 {
		return fail("missing secrets: "+strings.Join(missing, ", "), "add them to the environment (preferred) or the repository; values are never read", e)
	}
	return pass(strings.Join(required, ", ")+" present (names only)", e)
}

// tapTokenFromConfig reports whether every published brew reads its token
// from repository.token, whose secret name is not known here.
func (s *state) tapTokenFromConfig() bool {
	for _, b := range s.in.Config.Brews {
		if b.Repository.Token == "" {
			return false
		}
	}
	return len(s.in.Config.Brews) > 0
}

// activeCovering returns the active rulesets of target that cover ref.
func activeCovering(sets []github.Ruleset, target, ref, defaultBranch string) []github.Ruleset {
	var out []github.Ruleset
	for _, rs := range sets {
		if rs.Target == target && rs.Enforcement == "active" && refIncluded(rs, ref, defaultBranch) {
			out = append(out, rs)
		}
	}
	return out
}

// refIncluded evaluates a ruleset's ref_name condition (fnmatch patterns,
// ~ALL, ~DEFAULT_BRANCH).
func refIncluded(rs github.Ruleset, ref, defaultBranch string) bool {
	match := func(patterns []string) bool {
		for _, p := range patterns {
			switch p {
			case "~ALL":
				return true
			case "~DEFAULT_BRANCH":
				if defaultBranch != "" && ref == "refs/heads/"+defaultBranch {
					return true
				}
				continue
			}
			if ok, _ := path.Match(p, ref); ok {
				return true
			}
		}
		return false
	}
	c := rs.Conditions.RefName
	return match(c.Include) && !match(c.Exclude)
}

func ruleTypes(rs github.Ruleset) []string {
	var out []string
	for _, r := range rs.Rules {
		out = append(out, r.Type)
	}
	return out
}

func checkRulesetBranch(s *state) outcome {
	b, _, err := s.defaultBranch()
	if err != nil {
		return unknownf("release branch %v", err)
	}
	sets, err := s.getRulesets()
	if err != nil {
		return apiOutcome("rulesets", err)
	}
	cover := activeCovering(sets, "branch", "refs/heads/"+b, b)
	if len(cover) == 0 {
		return warn("no active ruleset protects "+b, "add a ruleset: pull request required, CI required, no force pushes or deletion (docs/preflight.md#recommended-settings)", ev{"branch": b})
	}
	var names, checks []string
	bypass := 0
	for _, rs := range cover {
		names = append(names, rs.Name)
		bypass += len(rs.BypassActors)
		for _, r := range rs.Rules {
			for _, c := range r.Parameters.RequiredStatusChecks {
				checks = append(checks, c.Context)
			}
		}
	}
	e := ev{"branch": b, "rulesets": names, "required_checks": checks, "bypass_actors": bypass}
	if len(checks) == 0 {
		return warn(fmt.Sprintf("%s protect %s but require no status check", strings.Join(names, ", "), b), "require the CI gate check", e)
	}
	return pass(fmt.Sprintf("%s: requires %s", strings.Join(names, ", "), strings.Join(checks, ", ")), e)
}

func checkRulesetTag(s *state) outcome {
	sets, err := s.getRulesets()
	if err != nil {
		return apiOutcome("rulesets", err)
	}
	ref := "refs/tags/" + s.in.Tag
	var protecting []string
	bypass := 0
	for _, rs := range activeCovering(sets, "tag", ref, "") {
		types := ruleTypes(rs)
		if slices.Contains(types, "creation") {
			return fail(fmt.Sprintf("ruleset %q forbids creating %s: the tag push would be refused", rs.Name, s.in.Tag),
				"remove the creation rule (keep deletion and update)", ev{"ruleset": rs.Name, "rules": types})
		}
		if slices.Contains(types, "deletion") && slices.Contains(types, "update") {
			protecting = append(protecting, rs.Name)
			bypass += len(rs.BypassActors)
		}
	}
	e := ev{"ref": ref, "rulesets": protecting, "bypass_actors": bypass}
	if len(protecting) == 0 {
		return warn("no active ruleset forbids moving or deleting "+ref, "add a tag ruleset on v* with deletion and update rules (docs/preflight.md#recommended-settings)", e)
	}
	return pass(strings.Join(protecting, ", ")+": update and deletion forbidden, creation allowed", e)
}

// --- tap ---

func checkTapRepository(s *state) outcome {
	if len(s.taps) == 0 {
		return skip("no formula is published to a tap for this tag")
	}
	var lines []string
	for _, t := range s.taps {
		r, err := s.getTapRepo(t.owner, t.name)
		if isNotFound(err) {
			return fail(t.owner+"/"+t.name+" was not found (or the token cannot see it)", "create the tap or fix brews[].repository", nil)
		}
		if err != nil {
			return apiOutcome("tap "+t.owner+"/"+t.name, err)
		}
		if r.Archived {
			return fail(r.FullName+" is archived", "unarchive the tap", nil)
		}
		lines = append(lines, fmt.Sprintf("%s (default branch %s)", r.FullName, r.DefaultBranch))
	}
	return pass(strings.Join(lines, "; "), nil)
}

func checkTapLayout(s *state) outcome {
	var lines []string
	for _, t := range s.taps {
		r, _ := s.getTapRepo(t.owner, t.name)
		entries, err := s.in.GitHub.ListDirectory(s.ctx, t.owner, t.name, "", r.DefaultBranch)
		if isNotFound(err) {
			entries, err = nil, nil
		}
		if err != nil {
			return apiOutcome("tap "+t.owner+"/"+t.name+" contents", err)
		}
		if err := tap.FormulaDirProblem(t.owner+"/"+t.name, entries, t.path); err != nil {
			return fail(err.Error(), "set brews[].directory to where the tap keeps its formulae", nil)
		}
		lines = append(lines, t.owner+"/"+t.name+":"+t.path)
	}
	return pass(strings.Join(lines, "; "), nil)
}

func checkTapBranch(s *state) outcome {
	var lines []string
	for _, t := range s.taps {
		if !t.pullRequest {
			lines = append(lines, t.formula+": direct commit, no branch")
			continue
		}
		_, err := s.in.GitHub.GetRef(s.ctx, t.owner, t.name, "heads/"+t.branch)
		switch {
		case err == nil:
			return fail(fmt.Sprintf("branch %s already exists on %s/%s", t.branch, t.owner, t.name),
				"delete the stale branch (after checking it is merged or unwanted) or release a new version", nil)
		case !isNotFound(err):
			return apiOutcome("tap branch "+t.branch, err)
		}
		lines = append(lines, fmt.Sprintf("%s/%s:%s is free", t.owner, t.name, t.branch))
	}
	return pass(strings.Join(lines, "; "), nil)
}

func checkTapPullRequest(s *state) outcome {
	var lines []string
	for _, t := range s.taps {
		if !t.pullRequest {
			continue
		}
		base := t.owner + "/" + t.name
		bo, bn := t.owner, t.name
		if t.baseOwner != "" && t.baseName != "" {
			bo, bn, base = t.baseOwner, t.baseName, t.baseOwner+"/"+t.baseName
		}
		prs, err := s.in.GitHub.ListPulls(s.ctx, bo, bn, github.ListPullsOptions{Head: t.owner + ":" + t.branch, State: "all"})
		if err != nil {
			return apiOutcome("pull requests of "+base, err)
		}
		for _, pr := range prs {
			if pr.State == "open" {
				return fail(fmt.Sprintf("pull request #%d from %s is still open on %s", pr.Number, t.branch, base),
					"merge or close it first", ev{"url": pr.HTMLURL})
			}
		}
		if len(prs) > 0 {
			return warn(fmt.Sprintf("an earlier pull request from %s exists on %s (#%d, %s)", t.branch, base, prs[0].Number, prs[0].State),
				"make sure this version was not already published to the tap", ev{"url": prs[0].HTMLURL})
		}
		lines = append(lines, fmt.Sprintf("none from %s on %s", t.branch, base))
	}
	if len(lines) == 0 {
		return skip("no formula is published by pull request")
	}
	return pass(strings.Join(lines, "; "), nil)
}
