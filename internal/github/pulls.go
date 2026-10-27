package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

// Pull request, branch rule and workflow run lookups, and the one
// conditional write the merge gate (internal/prgate) makes: MergePull.

// Pull is a pull request with the fields a merge decision needs.
type Pull struct {
	Number int    `json:"number"`
	State  string `json:"state"` // open or closed
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	// Mergeable is null while GitHub is still computing it.
	Mergeable *bool `json:"mergeable"`
	// MergeableState is clean, unstable, blocked, behind, dirty, draft,
	// has_hooks or unknown.
	MergeableState string `json:"mergeable_state"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	HTMLURL        string `json:"html_url"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

// GetPull returns a pull request.
func (c *Client) GetPull(ctx context.Context, owner, repo string, number int) (*Pull, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	var p Pull
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "pulls", strconv.Itoa(number)), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RequiredStatusCheck is a status check a branch rule or branch protection
// requires. IntegrationID 0 accepts any source.
type RequiredStatusCheck struct {
	Context       string `json:"context"`
	IntegrationID int64  `json:"integration_id"`
}

// BranchRule is one active rule that applies to a branch (GET
// rules/branches/{branch}): repository and organization rulesets alike.
type BranchRule struct {
	Type              string `json:"type"`
	RulesetID         int64  `json:"ruleset_id"`
	RulesetSourceType string `json:"ruleset_source_type"`
	RulesetSource     string `json:"ruleset_source"`
	// Parameters is decoded per type by the caller; only
	// required_status_checks is interpreted here (RequiredStatusChecks).
	Parameters json.RawMessage `json:"parameters"`
}

// RequiredStatusChecks decodes the parameters of a required_status_checks
// rule: the checks and whether the branch must be up to date (strict). A
// rule of another type returns nil, false, nil.
func (r BranchRule) RequiredStatusChecks() ([]RequiredStatusCheck, bool, error) {
	if r.Type != "required_status_checks" {
		return nil, false, nil
	}
	var p struct {
		Strict bool                  `json:"strict_required_status_checks_policy"`
		Checks []RequiredStatusCheck `json:"required_status_checks"`
	}
	if len(r.Parameters) == 0 {
		return nil, false, errors.New("github: required_status_checks rule without parameters")
	}
	if err := json.Unmarshal(r.Parameters, &p); err != nil {
		return nil, false, fmt.Errorf("github: required_status_checks rule: %w", err)
	}
	return p.Checks, p.Strict, nil
}

// GetBranchRules returns the active rules that apply to branch.
func (c *Client) GetBranchRules(ctx context.Context, owner, repo, branch string) ([]BranchRule, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	segs, err := splitPath("branch", branch)
	if err != nil {
		return nil, err
	}
	return listAll[BranchRule](ctx, c, repoURL(c.api, owner, repo, pageQuery(), append([]string{"rules", "branches"}, segs...)...))
}

// ProtectionCheck is a status check required by classic branch protection;
// a nil AppID accepts any source.
type ProtectionCheck struct {
	Context string `json:"context"`
	AppID   *int64 `json:"app_id"`
}

// Branch is a branch with its classic protection summary.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected  bool `json:"protected"`
	Protection *struct {
		Enabled              bool `json:"enabled"`
		RequiredStatusChecks *struct {
			EnforcementLevel string            `json:"enforcement_level"`
			Contexts         []string          `json:"contexts"`
			Checks           []ProtectionCheck `json:"checks"`
		} `json:"required_status_checks"`
	} `json:"protection"`
}

// GetBranch returns a branch, including the summary of its classic branch
// protection.
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch string) (*Branch, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	segs, err := splitPath("branch", branch)
	if err != nil {
		return nil, err
	}
	var b Branch
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, append([]string{"branches"}, segs...)...), nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// WorkflowRun is a GitHub Actions workflow run.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	WorkflowID int64  `json:"workflow_id"`
	Name       string `json:"name"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	RunAttempt int    `json:"run_attempt"`
	HTMLURL    string `json:"html_url"`
}

// ListWorkflowRunsForSHA returns every workflow run of a commit.
func (c *Client) ListWorkflowRunsForSHA(ctx context.Context, owner, repo, sha string) ([]WorkflowRun, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !IsFullSHA(sha) {
		return nil, fmt.Errorf("github: invalid commit %q", sha)
	}
	q := pageQuery()
	q.Set("head_sha", sha)
	return listWrapped(ctx, c, repoURL(c.api, owner, repo, q, "actions", "runs"),
		func(p *struct {
			Runs []WorkflowRun `json:"workflow_runs"`
		}) []WorkflowRun {
			return p.Runs
		})
}

// Commit is a commit with its parents.
type Commit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// GetCommit returns a commit (GET commits/{sha}).
func (c *Client) GetCommit(ctx context.Context, owner, repo, sha string) (*Commit, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !IsFullSHA(sha) {
		return nil, fmt.Errorf("github: invalid commit %q", sha)
	}
	var cm Commit
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "commits", sha), nil, &cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsFullSHA reports whether s is a full, lowercase hexadecimal SHA-1
// object name.
func IsFullSHA(s string) bool { return fullSHA.MatchString(s) }

// MergeResult is the response of MergePull.
type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// MergePull merges a pull request only if its head is still headSHA (PUT
// pulls/{n}/merge with "sha"); GitHub answers 409 when the head moved.
// method is merge, squash or rebase.
//
// It is sent exactly once, never retried: when the response is lost the
// merge may or may not have happened, so the caller re-reads the pull
// request instead of sending it again.
func (c *Client) MergePull(ctx context.Context, owner, repo string, number int, headSHA, method string) (*MergeResult, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	if !IsFullSHA(headSHA) {
		return nil, fmt.Errorf("github: merge: head %q is not a full commit SHA", headSHA)
	}
	switch method {
	case "merge", "squash", "rebase":
	default:
		return nil, fmt.Errorf("github: merge: invalid method %q", method)
	}
	body, err := json.Marshal(map[string]string{"sha": headSHA, "merge_method": method})
	if err != nil {
		return nil, fmt.Errorf("github: encoding request: %w", err)
	}
	var r MergeResult
	cl := &call{method: http.MethodPut, url: repoURL(c.api, owner, repo, nil, "pulls", strconv.Itoa(number), "merge"), body: body, handle: readJSON(&r)} //nolint:bodyclose // c.once closes the response body (drain) before returning
	if _, err := c.once(ctx, cl); err != nil {
		return nil, err
	}
	return &r, nil
}
