package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Repository is the subset of a repository the tap publisher needs.
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	HTMLURL       string `json:"html_url"`
}

// GetRepository returns a repository.
func (c *Client) GetRepository(ctx context.Context, owner, repo string) (*Repository, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	var r Repository
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// FileContent is a file read through the contents API.
type FileContent struct {
	Path string
	// SHA is the blob SHA, required by PutContents to update the file.
	SHA     string
	Content []byte
}

// GetContents reads a file (up to 1 MB, the contents API limit for inline
// content) at ref; an empty ref means the default branch. A missing file is
// an error matching ErrNotFound.
func (c *Client) GetContents(ctx context.Context, owner, repo, path, ref string) (*FileContent, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	segs, err := splitPath("path", path)
	if err != nil {
		return nil, err
	}
	var q url.Values
	if ref != "" {
		q = url.Values{"ref": {ref}}
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, q, append([]string{"contents"}, segs...)...), nil, &raw); err != nil {
		return nil, err
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
		return nil, fmt.Errorf("github: %s is a directory", path)
	}
	var r struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("github: invalid contents response: %w", err)
	}
	if r.Type != "file" {
		return nil, fmt.Errorf("github: %s is a %s, not a file", path, r.Type)
	}
	if r.Encoding != "base64" {
		return nil, fmt.Errorf("github: %s: unsupported content encoding %q (file too large for the contents API?)", path, r.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(r.Content))
	if err != nil {
		return nil, fmt.Errorf("github: %s: invalid base64 content: %w", path, err)
	}
	if int64(len(data)) != r.Size {
		return nil, fmt.Errorf("github: %s: decoded %d bytes, expected %d", path, len(data), r.Size)
	}
	return &FileContent{Path: r.Path, SHA: r.SHA, Content: data}, nil
}

// CommitAuthor identifies a commit author or committer.
type CommitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// PutContentsRequest creates or updates one file in one commit.
type PutContentsRequest struct {
	Message string
	Content []byte
	// SHA is the blob being replaced; required to update an existing file.
	// A stale SHA makes GitHub answer 409, which acts as an optimistic lock.
	SHA string
	// Branch defaults to the repository's default branch.
	Branch    string
	Author    *CommitAuthor
	Committer *CommitAuthor
}

// PutContentsResult describes the commit created by PutContents.
type PutContentsResult struct {
	ContentSHA string
	CommitSHA  string
	CommitURL  string
}

// PutContents creates or updates a file. Note that it is retried on 5xx
// like other PUTs; a retry after a lost success fails with 409 because the
// SHA moved, so callers should re-read and compare on conflict.
func (c *Client) PutContents(ctx context.Context, owner, repo, path string, req PutContentsRequest) (*PutContentsResult, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	segs, err := splitPath("path", path)
	if err != nil {
		return nil, err
	}
	if req.Message == "" {
		return nil, errors.New("github: put contents: commit message is required")
	}
	for _, a := range []*CommitAuthor{req.Author, req.Committer} {
		if a != nil && (a.Name == "" || a.Email == "") {
			return nil, errors.New("github: put contents: author and committer need a name and an email")
		}
	}
	body := struct {
		Message   string        `json:"message"`
		Content   string        `json:"content"`
		SHA       string        `json:"sha,omitempty"`
		Branch    string        `json:"branch,omitempty"`
		Author    *CommitAuthor `json:"author,omitempty"`
		Committer *CommitAuthor `json:"committer,omitempty"`
	}{req.Message, base64.StdEncoding.EncodeToString(req.Content), req.SHA, req.Branch, req.Author, req.Committer}
	var r struct {
		Content struct {
			SHA string `json:"sha"`
		} `json:"content"`
		Commit struct {
			SHA     string `json:"sha"`
			HTMLURL string `json:"html_url"`
		} `json:"commit"`
	}
	if err := c.doJSON(ctx, http.MethodPut, repoURL(c.api, owner, repo, nil, append([]string{"contents"}, segs...)...), body, &r); err != nil {
		return nil, err
	}
	return &PutContentsResult{ContentSHA: r.Content.SHA, CommitSHA: r.Commit.SHA, CommitURL: r.Commit.HTMLURL}, nil
}

// Reference is a git reference.
type Reference struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// GetRef returns a reference such as "heads/main" (a "refs/" prefix is
// accepted). A missing ref is an error matching ErrNotFound.
func (c *Client) GetRef(ctx context.Context, owner, repo, ref string) (*Reference, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	segs, err := splitPath("ref", strings.TrimPrefix(ref, "refs/"))
	if err != nil {
		return nil, err
	}
	var r Reference
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, append([]string{"git", "ref"}, segs...)...), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRef creates a reference (e.g. "refs/heads/x" or "heads/x") at sha.
// It is never retried automatically; a 422 "Reference already exists" may
// mean an earlier attempt succeeded.
func (c *Client) CreateRef(ctx context.Context, owner, repo, ref, sha string) (*Reference, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	full := ref
	if !strings.HasPrefix(full, "refs/") {
		full = "refs/" + full
	}
	if _, err := splitPath("ref", full); err != nil || strings.Count(full, "/") < 2 {
		return nil, fmt.Errorf("github: invalid ref %q", ref)
	}
	if sha == "" {
		return nil, errors.New("github: create ref: sha is required")
	}
	var r Reference
	body := map[string]string{"ref": full, "sha": sha}
	if err := c.doJSON(ctx, http.MethodPost, repoURL(c.api, owner, repo, nil, "git", "refs"), body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// PullRequest is the subset of a pull request the tap publisher needs.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref   string `json:"ref"`
		SHA   string `json:"sha"`
		Label string `json:"label"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// ListPullsOptions filters ListPulls. Head must be "owner:branch" for the
// filter to apply.
type ListPullsOptions struct {
	Head  string
	Base  string
	State string // open (default), closed or all
}

// ListPulls returns every pull request matching o.
func (c *Client) ListPulls(ctx context.Context, owner, repo string, o ListPullsOptions) ([]PullRequest, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if o.Head != "" && !strings.Contains(o.Head, ":") {
		return nil, fmt.Errorf("github: list pulls: head %q must be owner:branch", o.Head)
	}
	q := pageQuery()
	state := o.State
	if state == "" {
		state = "open"
	}
	q.Set("state", state)
	if o.Head != "" {
		q.Set("head", o.Head)
	}
	if o.Base != "" {
		q.Set("base", o.Base)
	}
	return listAll[PullRequest](ctx, c, repoURL(c.api, owner, repo, q, "pulls"))
}

// NewPullRequest is the body of CreatePull.
type NewPullRequest struct {
	Title               string `json:"title"`
	Head                string `json:"head"`
	Base                string `json:"base"`
	Body                string `json:"body,omitempty"`
	Draft               bool   `json:"draft,omitempty"`
	MaintainerCanModify *bool  `json:"maintainer_can_modify,omitempty"`
}

// CreatePull opens a pull request. It is never retried automatically; on
// failure, look for an existing one with ListPulls.
func (c *Client) CreatePull(ctx context.Context, owner, repo string, p NewPullRequest) (*PullRequest, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if p.Title == "" || p.Head == "" || p.Base == "" {
		return nil, errors.New("github: create pull request: title, head and base are required")
	}
	var r PullRequest
	if err := c.doJSON(ctx, http.MethodPost, repoURL(c.api, owner, repo, nil, "pulls"), p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CommitFile is one file changed in a comparison.
type CommitFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
}

// Comparison statuses.
const (
	CompareIdentical = "identical"
	CompareAhead     = "ahead"
	CompareBehind    = "behind"
	CompareDiverged  = "diverged"
)

// Comparison is the result of CompareCommits.
type Comparison struct {
	// Status is identical, ahead (head has commits base lacks), behind
	// (head is an ancestor of base) or diverged.
	Status       string `json:"status"`
	AheadBy      int    `json:"ahead_by"`
	BehindBy     int    `json:"behind_by"`
	TotalCommits int    `json:"total_commits"`
	// Files are the changes from the merge base to head. GitHub lists at
	// most 300 files.
	Files []CommitFile `json:"files"`
}

// CompareCommits compares base...head (three-dot: changes on head since the
// merge base). head may be "owner:branch" for a fork of the repository.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (*Comparison, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if base == "" || head == "" || strings.Contains(base, "..") || strings.Contains(head, "..") { // git ref names cannot contain ".."
		return nil, fmt.Errorf("github: invalid comparison %q...%q", base, head)
	}
	segs, err := splitPath("comparison", base+"..."+head)
	if err != nil {
		return nil, err
	}
	var r Comparison
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, append([]string{"compare"}, segs...)...), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Tag is an annotated tag object.
type Tag struct {
	SHA    string `json:"sha"`
	Tag    string `json:"tag"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// GetTag returns the annotated tag object sha (GET git/tags/{sha}); use it
// to peel a tag reference whose object type is "tag".
func (c *Client) GetTag(ctx context.Context, owner, repo, sha string) (*Tag, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if sha == "" || strings.ContainsAny(sha, "/?#%") {
		return nil, fmt.Errorf("github: invalid tag object %q", sha)
	}
	var t Tag
	if err := c.doJSON(ctx, http.MethodGet, repoURL(c.api, owner, repo, nil, "git", "tags", sha), nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
