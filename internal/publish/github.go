package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Repo is a GitHub repository.
type Repo struct{ Owner, Name string }

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// RepoInfo is what publish reads about a repository.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

// Workflow is a workflow file known to GitHub Actions.
type Workflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// Run is a workflow run.
type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Event      string    `json:"event"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	RunAttempt int       `json:"run_attempt"`
	CreatedAt  time.Time `json:"created_at"`
}

// Job is one job of a run, with its steps.
type Job struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Steps      []Step `json:"steps"`
}

// Step is one step of a job.
type Step struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Ruleset is a repository ruleset with its conditions and rules.
type Ruleset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []Rule `json:"rules"`
}

// Rule is one ruleset rule.
type Rule struct {
	Type       string `json:"type"`
	Parameters struct {
		RequiredStatusChecks []struct {
			Context string `json:"context"`
		} `json:"required_status_checks"`
	} `json:"parameters"`
}

// Environment is a deployment environment with its deployment policies.
type Environment struct {
	Name           string
	CustomPolicies bool
	Protected      bool // deployments limited to protected branches
	Policies       []EnvPolicy
}

// EnvPolicy is a deployment branch or tag policy.
type EnvPolicy struct {
	Name string `json:"name"`
	Type string `json:"type"` // "branch" or "tag"
}

// Release is a GitHub release.
type Release struct {
	ID         int64   `json:"id"`
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Body       string  `json:"body"`
	HTMLURL    string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Asset is a release asset. Digest is "sha256:<hex>" when GitHub has
// computed it.
type Asset struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Content is a directory entry of a repository.
type Content struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file", "dir", ...
}

// PullRequest is a pull request.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Merged  bool   `json:"-"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	MergedAt *time.Time `json:"merged_at"`
}

// AttestPolicy is what an attestation must prove.
type AttestPolicy struct {
	Repo           Repo
	SignerWorkflow string // owner/repo/.github/workflows/release.yml
	SourceRef      string // refs/tags/v1.2.3
	SourceDigest   string // the tagged commit
}

// GitHub is the GitHub access publish needs. Everything is read-only:
// publish never writes to GitHub through this interface (the tag push is
// a git operation, and the release workflow does the rest).
type GitHub interface {
	AuthStatus(ctx context.Context) error
	Repo(ctx context.Context, r Repo) (RepoInfo, error)
	ActionsEnabled(ctx context.Context, r Repo) (bool, error)
	Workflow(ctx context.Context, r Repo, file string) (Workflow, error)
	// Runs lists the runs of a workflow file filtered by event, branch (for
	// a tag push: the tag name) and head SHA, newest first.
	Runs(ctx context.Context, r Repo, file, event, branch, sha string) ([]Run, error)
	RunByID(ctx context.Context, r Repo, id int64) (Run, error)
	Jobs(ctx context.Context, r Repo, runID int64) ([]Job, error)
	Rulesets(ctx context.Context, r Repo) ([]Ruleset, error)
	// Environment returns ErrNotFound when the environment does not exist.
	Environment(ctx context.Context, r Repo, name string) (Environment, error)
	// EnvironmentSecrets lists secret names, never values.
	EnvironmentSecrets(ctx context.Context, r Repo, env string) ([]string, error)
	// Releases lists releases, drafts included.
	Releases(ctx context.Context, r Repo) ([]Release, error)
	// LatestRelease returns ErrNotFound when no release is "latest".
	LatestRelease(ctx context.Context, r Repo) (Release, error)
	Contents(ctx context.Context, r Repo, dir, ref string) ([]Content, error)
	// File returns ErrNotFound when the file does not exist.
	File(ctx context.Context, r Repo, path, ref string) ([]byte, error)
	BranchHead(ctx context.Context, r Repo, branch string) (string, error)
	// PullRequests lists pull requests (any state) whose head is
	// owner:branch.
	PullRequests(ctx context.Context, r Repo, headOwner, branch string) ([]PullRequest, error)
	PullRequestFiles(ctx context.Context, r Repo, number int) ([]string, error)
	DownloadRelease(ctx context.Context, r Repo, tag, dir string) error
	VerifyAttestation(ctx context.Context, file string, p AttestPolicy) error
}

// GHCLI implements GitHub with the gh command. Transient failures (network,
// 5xx, rate limits) are retried with Sleep between attempts.
type GHCLI struct {
	Run   Runner
	Sleep func(ctx context.Context, d time.Duration) error
	// Attempts per API call (default 4).
	Attempts int
}

func (g GHCLI) gh(ctx context.Context, args ...string) ([]byte, error) {
	attempts := g.Attempts
	if attempts <= 0 {
		attempts = 4
	}
	delay := 2 * time.Second
	for i := 1; ; i++ {
		out, err := g.Run(ctx, "gh", args...)
		if err == nil {
			return out, nil
		}
		if httpStatus(err) == 404 {
			return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		if i >= attempts || !transient(err) || g.Sleep == nil {
			return nil, err
		}
		if serr := g.Sleep(ctx, delay); serr != nil {
			return nil, serr
		}
		delay *= 2
	}
}

func (g GHCLI) api(ctx context.Context, path string, out any) error {
	b, err := g.gh(ctx, "api", "--method", "GET",
		"-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28", path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

func repoPath(r Repo) string {
	return "repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func (g GHCLI) AuthStatus(ctx context.Context) error {
	_, err := g.Run(ctx, "gh", "auth", "status", "--hostname", "github.com")
	return err
}

func (g GHCLI) Repo(ctx context.Context, r Repo) (RepoInfo, error) {
	var info RepoInfo
	return info, g.api(ctx, repoPath(r), &info)
}

func (g GHCLI) ActionsEnabled(ctx context.Context, r Repo) (bool, error) {
	var p struct {
		Enabled bool `json:"enabled"`
	}
	err := g.api(ctx, repoPath(r)+"/actions/permissions", &p)
	return p.Enabled, err
}

func (g GHCLI) Workflow(ctx context.Context, r Repo, file string) (Workflow, error) {
	var w Workflow
	return w, g.api(ctx, repoPath(r)+"/actions/workflows/"+url.PathEscape(file), &w)
}

func (g GHCLI) Runs(ctx context.Context, r Repo, file, event, branch, sha string) ([]Run, error) {
	q := url.Values{"per_page": {"50"}}
	if event != "" {
		q.Set("event", event)
	}
	if branch != "" {
		q.Set("branch", branch)
	}
	if sha != "" {
		q.Set("head_sha", sha)
	}
	var res struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := g.api(ctx, repoPath(r)+"/actions/workflows/"+url.PathEscape(file)+"/runs?"+q.Encode(), &res)
	return res.Runs, err
}

func (g GHCLI) RunByID(ctx context.Context, r Repo, id int64) (Run, error) {
	var run Run
	return run, g.api(ctx, fmt.Sprintf("%s/actions/runs/%d", repoPath(r), id), &run)
}

func (g GHCLI) Jobs(ctx context.Context, r Repo, runID int64) ([]Job, error) {
	var res struct {
		Jobs []Job `json:"jobs"`
	}
	err := g.api(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", repoPath(r), runID), &res)
	return res.Jobs, err
}

func (g GHCLI) Rulesets(ctx context.Context, r Repo) ([]Ruleset, error) {
	var list []Ruleset
	if err := g.api(ctx, repoPath(r)+"/rulesets?includes_parents=true&per_page=100", &list); err != nil {
		return nil, err
	}
	out := make([]Ruleset, 0, len(list))
	for _, rs := range list {
		var full Ruleset
		if err := g.api(ctx, fmt.Sprintf("%s/rulesets/%d", repoPath(r), rs.ID), &full); err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

func (g GHCLI) Environment(ctx context.Context, r Repo, name string) (Environment, error) {
	var env struct {
		Name   string `json:"name"`
		Policy *struct {
			Protected bool `json:"protected_branches"`
			Custom    bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
	}
	p := repoPath(r) + "/environments/" + url.PathEscape(name)
	if err := g.api(ctx, p, &env); err != nil {
		return Environment{}, err
	}
	e := Environment{Name: env.Name}
	if env.Policy == nil {
		return e, nil
	}
	e.Protected, e.CustomPolicies = env.Policy.Protected, env.Policy.Custom
	if e.CustomPolicies {
		var pol struct {
			Policies []EnvPolicy `json:"branch_policies"`
		}
		if err := g.api(ctx, p+"/deployment-branch-policies?per_page=100", &pol); err != nil {
			return Environment{}, err
		}
		e.Policies = pol.Policies
	}
	return e, nil
}

func (g GHCLI) EnvironmentSecrets(ctx context.Context, r Repo, env string) ([]string, error) {
	var res struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := g.api(ctx, repoPath(r)+"/environments/"+url.PathEscape(env)+"/secrets?per_page=100", &res); err != nil {
		return nil, err
	}
	var names []string
	for _, s := range res.Secrets {
		names = append(names, s.Name)
	}
	return names, nil
}

func (g GHCLI) Releases(ctx context.Context, r Repo) ([]Release, error) {
	var rels []Release
	return rels, g.api(ctx, repoPath(r)+"/releases?per_page=100", &rels)
}

func (g GHCLI) LatestRelease(ctx context.Context, r Repo) (Release, error) {
	var rel Release
	return rel, g.api(ctx, repoPath(r)+"/releases/latest", &rel)
}

func (g GHCLI) Contents(ctx context.Context, r Repo, dir, ref string) ([]Content, error) {
	var list []Content
	p := repoPath(r) + "/contents/" + strings.TrimPrefix(dir, "/")
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	return list, g.api(ctx, p, &list)
}

func (g GHCLI) File(ctx context.Context, r Repo, path, ref string) ([]byte, error) {
	p := repoPath(r) + "/contents/" + strings.TrimPrefix(path, "/")
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	return g.gh(ctx, "api", "--method", "GET", "-H", "Accept: application/vnd.github.raw", p)
}

func (g GHCLI) BranchHead(ctx context.Context, r Repo, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.api(ctx, repoPath(r)+"/git/ref/heads/"+url.PathEscape(branch), &ref)
	return ref.Object.SHA, err
}

func (g GHCLI) PullRequests(ctx context.Context, r Repo, headOwner, branch string) ([]PullRequest, error) {
	q := url.Values{"state": {"all"}, "head": {headOwner + ":" + branch}, "per_page": {"100"}}
	var prs []PullRequest
	if err := g.api(ctx, repoPath(r)+"/pulls?"+q.Encode(), &prs); err != nil {
		return nil, err
	}
	for i := range prs {
		prs[i].Merged = prs[i].MergedAt != nil
	}
	return prs, nil
}

func (g GHCLI) PullRequestFiles(ctx context.Context, r Repo, number int) ([]string, error) {
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := g.api(ctx, fmt.Sprintf("%s/pulls/%d/files?per_page=100", repoPath(r), number), &files); err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Filename)
	}
	return out, nil
}

func (g GHCLI) DownloadRelease(ctx context.Context, r Repo, tag, dir string) error {
	_, err := g.gh(ctx, "release", "download", tag, "--repo", r.String(), "--dir", dir, "--clobber")
	return err
}

func (g GHCLI) VerifyAttestation(ctx context.Context, file string, p AttestPolicy) error {
	if p.Repo.Owner == "" || p.SignerWorkflow == "" || p.SourceRef == "" || p.SourceDigest == "" {
		return errors.New("attestation policy is incomplete")
	}
	_, err := g.Run(ctx, "gh", "attestation", "verify", file,
		"--repo", p.Repo.String(),
		"--signer-workflow", p.SignerWorkflow,
		"--source-ref", p.SourceRef,
		"--source-digest", p.SourceDigest,
		"--deny-self-hosted-runners",
		"--format", "json")
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	return nil
}

// Sleep sleeps for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
