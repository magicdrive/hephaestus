package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Read-only lookups of repository settings and commit status, used by the
// release preflight (internal/preflight). Every method here is a GET.

// Environment is a deployment environment.
type Environment struct {
	Name                   string `json:"name"`
	DeploymentBranchPolicy *struct {
		ProtectedBranches    bool `json:"protected_branches"`
		CustomBranchPolicies bool `json:"custom_branch_policies"`
	} `json:"deployment_branch_policy"`
}

// GetEnvironment returns an environment; a missing one matches ErrNotFound.
func (c *Client) GetEnvironment(ctx context.Context, owner, repo, name string) (*Environment, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var e Environment
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "environments", name), nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// DeploymentPolicy is a custom deployment branch or tag policy.
type DeploymentPolicy struct {
	Name string `json:"name"`
	Type string `json:"type"` // "branch" or "tag"
}

// ListDeploymentPolicies returns an environment's custom deployment policies.
func (c *Client) ListDeploymentPolicies(ctx context.Context, owner, repo, env string) ([]DeploymentPolicy, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return listWrapped(ctx, c, repoURL(c.api, owner, repo, pageQuery(), "environments", env, "deployment-branch-policies"),
		func(p *struct {
			Policies []DeploymentPolicy `json:"branch_policies"`
		}) []DeploymentPolicy {
			return p.Policies
		})
}

type secretList struct {
	Secrets []struct {
		Name string `json:"name"`
	} `json:"secrets"`
}

func secretNames(p *secretList) []string {
	out := make([]string, 0, len(p.Secrets))
	for _, s := range p.Secrets {
		out = append(out, s.Name)
	}
	return out
}

// ListEnvironmentSecretNames returns the names (never the values) of an
// environment's secrets. GitHub requires admin access or a token with the
// secrets read permission.
func (c *Client) ListEnvironmentSecretNames(ctx context.Context, owner, repo, env string) ([]string, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return listWrapped(ctx, c, repoURL(c.api, owner, repo, pageQuery(), "environments", env, "secrets"), secretNames)
}

// ListRepositorySecretNames returns the names of the repository's Actions
// secrets.
func (c *Client) ListRepositorySecretNames(ctx context.Context, owner, repo string) ([]string, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return listWrapped(ctx, c, repoURL(c.api, owner, repo, pageQuery(), "actions", "secrets"), secretNames)
}

// Ruleset is a repository ruleset (rules are present in GetRuleset's
// result, not in ListRulesets').
type Ruleset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Target      string `json:"target"`      // "branch", "tag", "push"
	Enforcement string `json:"enforcement"` // "active", "evaluate", "disabled"
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	BypassActors []struct {
		ActorType  string `json:"actor_type"`
		BypassMode string `json:"bypass_mode"`
	} `json:"bypass_actors"`
	Rules []RulesetRule `json:"rules"`
}

// RulesetRule is one rule of a ruleset.
type RulesetRule struct {
	Type       string `json:"type"`
	Parameters struct {
		RequiredStatusChecks []struct {
			Context       string `json:"context"`
			IntegrationID int64  `json:"integration_id"`
		} `json:"required_status_checks"`
	} `json:"parameters"`
}

// ListRulesets returns the rulesets that apply to the repository, including
// those inherited from the organization.
func (c *Client) ListRulesets(ctx context.Context, owner, repo string) ([]Ruleset, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	q := pageQuery()
	q.Set("includes_parents", "true")
	return listAll[Ruleset](ctx, c, repoURL(c.api, owner, repo, q, "rulesets"))
}

// GetRuleset returns one ruleset with its rules.
func (c *Client) GetRuleset(ctx context.Context, owner, repo string, id int64) (*Ruleset, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var r Ruleset
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "rulesets", strconv.FormatInt(id, 10)), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CheckRun is a check run on a commit.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued, in_progress, completed, ...
	Conclusion string `json:"conclusion"` // success, failure, neutral, cancelled, skipped, timed_out, action_required, stale
	HTMLURL    string `json:"html_url"`
	App        *struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	} `json:"app"`
}

// ListCheckRuns returns the check runs of a commit.
func (c *Client) ListCheckRuns(ctx context.Context, owner, repo, sha string) ([]CheckRun, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	return listWrapped(ctx, c, repoURL(c.api, owner, repo, pageQuery(), "commits", sha, "check-runs"),
		func(p *struct {
			CheckRuns []CheckRun `json:"check_runs"`
		}) []CheckRun {
			return p.CheckRuns
		})
}

// CombinedStatus is the combined commit status (the statuses API, used by
// external CI services that do not create check runs).
type CombinedStatus struct {
	State      string `json:"state"` // success, pending, failure
	TotalCount int    `json:"total_count"`
	Statuses   []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

// GetCombinedStatus returns the combined status of a commit.
func (c *Client) GetCombinedStatus(ctx context.Context, owner, repo, sha string) (*CombinedStatus, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var s CombinedStatus
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, pageQuery(), "commits", sha, "status"), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Workflow is a GitHub Actions workflow.
type Workflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"` // active, disabled_manually, ...
}

// GetWorkflow returns a workflow by file name (e.g. "release.yml").
func (c *Client) GetWorkflow(ctx context.Context, owner, repo, file string) (*Workflow, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var w Workflow
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "actions", "workflows", file), nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// listWrapped is listAll for endpoints that wrap each page's items in an
// object ({"total_count": n, "<field>": [...]}).
func listWrapped[P any, T any](ctx context.Context, c *Client, first string, items func(*P) []T) ([]T, error) {
	out := []T{}
	next := first
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("github: GET %s: more than %d pages", first, maxPages)
		}
		var p P
		h, err := c.do(ctx, &call{method: http.MethodGet, url: next, handle: readJSON(&p)}) //nolint:bodyclose // c.do closes the response body (drain) before returning
		if err != nil {
			return nil, err
		}
		out = append(out, items(&p)...)
		next = nextLink(h.Get("Link"))
		if next != "" {
			u, err := url.Parse(next)
			if err != nil || !sameOrigin(u, c.api) {
				return nil, fmt.Errorf("github: GET %s: refusing pagination link outside the API origin", first)
			}
		}
	}
	return out, nil
}
