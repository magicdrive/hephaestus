package preflight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/version"
)

// GitHub is the read-only GitHub access the preflight needs. Every method
// is a GET; *github.Client implements it.
type GitHub interface {
	GetRepository(ctx context.Context, owner, repo string) (*github.Repository, error)
	ListReleases(ctx context.Context, owner, repo string) ([]github.Release, error)
	ListCheckRuns(ctx context.Context, owner, repo, sha string) ([]github.CheckRun, error)
	GetCombinedStatus(ctx context.Context, owner, repo, sha string) (*github.CombinedStatus, error)
	GetWorkflow(ctx context.Context, owner, repo, file string) (*github.Workflow, error)
	GetEnvironment(ctx context.Context, owner, repo, name string) (*github.Environment, error)
	ListDeploymentPolicies(ctx context.Context, owner, repo, env string) ([]github.DeploymentPolicy, error)
	ListEnvironmentSecretNames(ctx context.Context, owner, repo, env string) ([]string, error)
	ListRepositorySecretNames(ctx context.Context, owner, repo string) ([]string, error)
	ListRulesets(ctx context.Context, owner, repo string) ([]github.Ruleset, error)
	GetRuleset(ctx context.Context, owner, repo string, id int64) (*github.Ruleset, error)
	ListDirectory(ctx context.Context, owner, repo, path, ref string) ([]github.DirEntry, error)
	GetRef(ctx context.Context, owner, repo, ref string) (*github.Reference, error)
	ListPulls(ctx context.Context, owner, repo string, o github.ListPullsOptions) ([]github.PullRequest, error)
}

var _ GitHub = (*github.Client)(nil)

// DefaultTapTokenSecret is the secret name of the tap token.
const DefaultTapTokenSecret = "HOMEBREW_TAP_GITHUB_TOKEN" //nolint:gosec // a secret name, not a credential

// settings are the effective preflight settings: flags over the
// configuration's preflight section over defaults.
type settings struct {
	remote, branch, workflow, environment, tapTokenSecret string
	secrets, skip                                         []string
}

func resolveSettings(in Input) settings {
	var s settings
	if in.Config != nil {
		p := in.Config.Preflight
		s = settings{remote: p.Remote, branch: p.Branch, workflow: p.Workflow, environment: p.Environment,
			tapTokenSecret: p.TapTokenSecret, secrets: p.Secrets, skip: p.Skip}
	}
	for _, o := range []struct {
		dst *string
		v   string
	}{
		{&s.remote, in.Remote}, {&s.branch, in.Branch}, {&s.workflow, in.Workflow},
		{&s.environment, in.Environment}, {&s.tapTokenSecret, in.TapTokenSecret},
	} {
		if o.v != "" {
			*o.dst = o.v
		}
	}
	s.secrets = append(append([]string(nil), s.secrets...), in.Secrets...)
	s.skip = append(append([]string(nil), s.skip...), in.Skip...)
	if s.remote == "" {
		s.remote = "origin"
	}
	if s.tapTokenSecret == "" {
		s.tapTokenSecret = DefaultTapTokenSecret
	}
	return s
}

// lazy memoizes one read for the whole run, so every check sees the same
// answer and no API is called twice.
type lazy[T any] struct {
	done bool
	v    T
	err  error
}

func (l *lazy[T]) get(f func() (T, error)) (T, error) {
	if !l.done {
		l.v, l.err = f()
		l.done = true
	}
	return l.v, l.err
}

// state is one run: inputs, facts established by earlier checks and
// memoized reads.
type state struct {
	ctx      context.Context
	in       Input
	settings settings
	results  map[string]Result

	// Facts.
	owner, repo string // release repository, "" when unknown
	repoSource  string
	head        string
	branch      string // current branch
	info        version.Info
	tagOK       bool
	prerelease  bool
	taps        []tapPlan

	remoteRefs lazy[map[string]string]
	repoInfo   lazy[*github.Repository]
	rulesets   lazy[[]github.Ruleset]
	defBranch  lazy[string]
	releases   lazy[[]github.Release]
	tapRepos   map[string]*lazy[*github.Repository]
}

func newState(ctx context.Context, in Input) *state {
	s := &state{ctx: ctx, in: in, settings: resolveSettings(in), results: map[string]Result{}, tapRepos: map[string]*lazy[*github.Repository]{}}
	s.resolveRepo()
	return s
}

// resolveRepo finds the release repository: --repo, then
// release.github.owner/name, then the git remote.
func (s *state) resolveRepo() {
	split := func(v string) (string, string, bool) {
		o, r, ok := strings.Cut(v, "/")
		return o, r, ok && o != "" && r != "" && !strings.Contains(r, "/")
	}
	if o, r, ok := split(s.in.Repo); ok {
		s.owner, s.repo, s.repoSource = o, r, "--repo"
		return
	}
	if c := s.in.Config; c != nil && c.Release.GitHub.IsEnabled() && c.Release.GitHub.Owner != "" && c.Release.GitHub.Repo != "" {
		s.owner, s.repo, s.repoSource = c.Release.GitHub.Owner, c.Release.GitHub.Repo, "release.github"
		return
	}
	if s.in.Git == nil {
		return
	}
	if u, err := s.in.Git.RemoteURL(s.ctx, s.settings.remote, false); err == nil {
		if _, o, r, err := github.ParseRemoteURL(u); err == nil {
			s.owner, s.repo, s.repoSource = o, r, "remote "+s.settings.remote
		}
	}
}

func (s *state) repoFullName() string {
	if s.owner == "" {
		return ""
	}
	return s.owner + "/" + s.repo
}

func (s *state) getRepoInfo() (*github.Repository, error) {
	return s.repoInfo.get(func() (*github.Repository, error) {
		return s.in.GitHub.GetRepository(s.ctx, s.owner, s.repo)
	})
}

// defaultBranch is the branch releases are tagged on: the configured one,
// else GitHub's default branch, else refs/remotes/<remote>/HEAD.
func (s *state) defaultBranch() (string, string, error) {
	if s.settings.branch != "" {
		return s.settings.branch, "configured", nil
	}
	if s.in.GitHub != nil && s.owner != "" {
		if r, err := s.getRepoInfo(); err == nil && r.DefaultBranch != "" {
			return r.DefaultBranch, "GitHub default branch", nil
		}
	}
	b, err := s.defBranch.get(func() (string, error) {
		return s.in.Git.RemoteDefaultBranch(s.ctx, s.settings.remote)
	})
	if err != nil {
		return "", "", err
	}
	if b == "" {
		return "", "", errors.New("unknown: set preflight.branch or --branch, or run git remote set-head " + s.settings.remote + " --auto")
	}
	return b, "refs/remotes/" + s.settings.remote + "/HEAD", nil
}

func (s *state) getRemoteRefs() (map[string]string, error) {
	return s.remoteRefs.get(func() (map[string]string, error) {
		return s.in.Git.LsRemote(s.ctx, s.settings.remote)
	})
}

func (s *state) getRulesets() ([]github.Ruleset, error) {
	return s.rulesets.get(func() ([]github.Ruleset, error) {
		list, err := s.in.GitHub.ListRulesets(s.ctx, s.owner, s.repo)
		if err != nil {
			return nil, err
		}
		full := make([]github.Ruleset, 0, len(list))
		for _, r := range list {
			if r.Enforcement != "active" {
				full = append(full, r) // rules not needed for inactive ones
				continue
			}
			f, err := s.in.GitHub.GetRuleset(s.ctx, s.owner, s.repo, r.ID)
			if err != nil {
				return nil, err
			}
			full = append(full, *f)
		}
		return full, nil
	})
}

func (s *state) getReleases() ([]github.Release, error) {
	return s.releases.get(func() ([]github.Release, error) {
		return s.in.GitHub.ListReleases(s.ctx, s.owner, s.repo)
	})
}

func (s *state) getTapRepo(owner, name string) (*github.Repository, error) {
	k := owner + "/" + name
	l, ok := s.tapRepos[k]
	if !ok {
		l = &lazy[*github.Repository]{}
		s.tapRepos[k] = l
	}
	return l.get(func() (*github.Repository, error) { return s.in.GitHub.GetRepository(s.ctx, owner, name) })
}

// apiOutcome turns a GitHub error into UNKNOWN (could not check) with a
// hint, keeping "not found" distinguishable for callers that test it first.
func apiOutcome(what string, err error) outcome {
	o := unknownf("could not read %s: %v", what, err)
	switch code := github.StatusCode(err); {
	case code == http.StatusUnauthorized:
		o.fix = "set a valid token in GITHUB_TOKEN (or --token-env)"
	case code == http.StatusForbidden || code == http.StatusNotFound:
		o.fix = "the token cannot read this; see docs/preflight.md#permissions"
	case code == http.StatusTooManyRequests:
		o.fix = "rate limited: retry later, or use a token"
	case code >= 500:
		o.fix = "GitHub error: retry"
	default:
		o.fix = "network or transport error: retry"
	}
	var ae *github.APIError
	if errors.As(err, &ae) && ae.RateLimited() {
		o.fix = "rate limited: retry later, or use a token"
	}
	return o
}

func isNotFound(err error) bool { return errors.Is(err, github.ErrNotFound) }

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
