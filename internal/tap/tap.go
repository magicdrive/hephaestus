// Package tap publishes generated Homebrew formulae to tap repositories
// through the GitHub contents, refs and pulls APIs.
//
// Two modes are supported, resolved by homebrew.ResolvePublish:
//
//   - Pull request mode (the default). The formula is committed to a head
//     branch (default "{{ .ProjectName }}-{{ .Version }}", as GoReleaser) that
//     is created from the base branch when missing, and a pull request is
//     opened against the base (which may be another repository, e.g. the
//     upstream of a fork). An open pull request for the same head is reused.
//   - Direct mode (repository.pull_request.enabled: false). The formula is
//     committed to repository.branch, or the default branch.
//
// Every write is preceded by a read: identical content is a no-op, so a
// re-run creates no commit and no pull request. Commits use the contents
// API with the current blob SHA as an optimistic lock; a conflict is
// re-read and retried a limited number of times. References are only ever
// created, never moved: there is no force update anywhere in this package.
//
// Commit author: when commit_author is unset the commit is attributed to the
// token's identity (GitHub's default for API commits). Unlike GoReleaser,
// which defaults to "goreleaserbot <bot@goreleaser.com>", Hephaestus does not
// impersonate a bot account.
package tap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Modes.
const (
	ModePullRequest = "pull_request"
	ModeDirect      = "direct"
)

// Statuses.
const (
	StatusCreated   = "created"   // the formula is new in the tap
	StatusUpdated   = "updated"   // the formula was changed
	StatusUnchanged = "unchanged" // already up to date; nothing written
	StatusPlanned   = "planned"   // dry run: would be created or updated
	StatusSkipped   = "skipped"   // skip_upload
	StatusFailed    = "failed"
)

// TokenEnv is the environment variable conventionally holding the tap token.
const TokenEnv = "HOMEBREW_TAP_GITHUB_TOKEN"

const (
	maxFormulaSize = 1 << 20 // the contents API returns inline content up to 1 MB
	commitAttempts = 3
)

// Input configures Run.
type Input struct {
	Config   *config.Config
	Manifest *manifest.Manifest
	Vars     tmpl.Vars
	// Client is the default tap client (HOMEBREW_TAP_GITHUB_TOKEN); nil when
	// there is no token.
	Client *github.Client
	// ClientFor builds a client for a brews[].repository.token.
	ClientFor func(token string) *github.Client
	DryRun    bool
	Log       io.Writer
}

// Result is the outcome for one formula.
type Result struct {
	Formula        string
	Repository     string
	Mode           string
	Status         string
	Branch         string
	PullRequestURL string
	CommitSHA      string
	// NoOp reports that nothing was (dry run: would be) written.
	NoOp bool
	// Err is set when Status is failed.
	Err error `json:"-"`
}

// Run publishes every configured formula. All formulae are attempted; the
// error, if any, joins the failures, and the results distinguish them.
func Run(ctx context.Context, in Input) ([]Result, error) {
	if in.Config == nil || in.Manifest == nil {
		return nil, errors.New("tap: config and manifest are required")
	}
	log := in.Log
	if log == nil {
		log = io.Discard
	}
	var results []Result
	var errs []error
	for i := range in.Config.Brews {
		b := &in.Config.Brews[i]
		p := &publisher{in: in, b: b, log: log}
		res := p.run(ctx)
		if res.Err != nil {
			res.Status = StatusFailed
			res.NoOp = false
			errs = append(errs, fmt.Errorf("formula %s: %w", b.Name, res.Err))
			fmt.Fprintf(log, "tap: formula %s: %v\n", b.Name, res.Err)
		}
		results = append(results, res)
	}
	if len(errs) > 0 {
		return results, fmt.Errorf("tap: %d of %d formulae failed: %w", len(errs), len(results), errors.Join(errs...))
	}
	return results, nil
}

type publisher struct {
	in      Input
	b       *config.Brew
	log     io.Writer
	p       homebrew.Publish
	c       *github.Client
	content []byte
	res     Result
}

func (p *publisher) logf(format string, args ...any) {
	prefix := "tap: "
	if p.in.DryRun {
		prefix = "tap: [dry-run] "
	}
	fmt.Fprintf(p.log, prefix+p.b.Name+": "+format+"\n", args...)
}

func (p *publisher) fail(err error) Result {
	p.res.Err = err
	return p.res
}

func (p *publisher) run(ctx context.Context) Result {
	p.res = Result{Formula: p.b.Name}
	pub, err := homebrew.ResolvePublish(p.b, p.in.Vars)
	if err != nil {
		return p.fail(err)
	}
	p.p = pub
	p.res.Repository = pub.Owner + "/" + pub.Name
	p.res.Mode = ModeDirect
	if pub.PullRequest {
		p.res.Mode = ModePullRequest
	}
	p.res.Branch = pub.Branch
	if pub.Skip {
		p.res.Status, p.res.NoOp = StatusSkipped, true
		p.logf("skipped: %s", pub.SkipReason)
		return p.res
	}
	if p.content, err = p.readFormula(); err != nil {
		return p.fail(err)
	}
	if p.c, err = p.client(); err != nil {
		return p.fail(err)
	}
	if p.c == nil { // dry run without a token: nothing can be compared
		p.res.Status = StatusPlanned
		p.logf("would publish %s to %s (%s mode); no token, so the tap was not read", pub.Path, p.res.Repository, p.res.Mode)
		return p.res
	}
	if pub.PullRequest {
		err = p.pullRequest(ctx)
	} else {
		err = p.direct(ctx)
	}
	if err != nil {
		return p.fail(err)
	}
	return p.res
}

// readFormula reads the generated formula and checks it against the
// manifest.
func (p *publisher) readFormula() ([]byte, error) {
	var art *manifest.Artifact
	for i, a := range p.in.Manifest.Artifacts {
		if a.Kind == manifest.KindFormula && (a.ID == p.b.Name || (a.ID == "" && a.Name == p.b.Name+".rb")) {
			art = &p.in.Manifest.Artifacts[i]
			break
		}
	}
	if art == nil {
		return nil, errors.New("the manifest has no generated formula; run the formula stage first")
	}
	if art.SHA256 == "" {
		return nil, errors.New("the manifest records no sha256 for the formula")
	}
	path := filepath.Join(p.in.Config.Root, filepath.FromSlash(art.Path))
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFormulaSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFormulaSize {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxFormulaSize)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != art.SHA256 || int64(len(data)) != art.Size {
		return nil, fmt.Errorf("%s does not match the manifest (sha256 %s, %d bytes; manifest %s, %d bytes): modified after it was generated?", path, got, len(data), art.SHA256, art.Size)
	}
	return data, nil
}

// client selects the client: repository.token when set, else the default.
func (p *publisher) client() (*github.Client, error) {
	tok, err := p.p.Token(p.in.Vars)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		if p.in.ClientFor == nil {
			return nil, errors.New("repository.token is set but no client factory was provided")
		}
		if c := p.in.ClientFor(tok); c != nil {
			return c, nil
		}
		return nil, errors.New("could not create a client for repository.token")
	}
	if p.in.Client != nil {
		return p.in.Client, nil
	}
	if p.in.DryRun {
		return nil, nil
	}
	return nil, fmt.Errorf("no token to write to %s: set %s (or brews[].repository.token) to a GitHub App installation token or a fine-grained PAT with contents and pull-requests write access to the tap; GITHUB_TOKEN cannot write to other repositories", p.res.Repository, TokenEnv)
}

// hint adds token advice to authorization failures.
func hint(err error, repo string) error {
	switch github.StatusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return fmt.Errorf("%w (does the token have write access to %s? GITHUB_TOKEN is limited to the workflow's own repository; use %s with a GitHub App token or fine-grained PAT)", err, repo, TokenEnv)
	}
	return err
}

func (p *publisher) author() *github.CommitAuthor {
	if p.p.AuthorName == "" {
		return nil
	}
	return &github.CommitAuthor{Name: p.p.AuthorName, Email: p.p.AuthorEmail}
}

// current reads the formula on branch; a missing file is (nil, nil).
func (p *publisher) current(ctx context.Context, owner, repo, branch string) (*github.FileContent, error) {
	fc, err := p.c.GetContents(ctx, owner, repo, p.p.Path, branch)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, hint(fmt.Errorf("reading %s on %s/%s@%s: %w", p.p.Path, owner, repo, branch, err), owner+"/"+repo)
	}
	return fc, nil
}

func (p *publisher) defaultBranch(ctx context.Context, owner, repo string) (string, error) {
	r, err := p.c.GetRepository(ctx, owner, repo)
	if err != nil {
		return "", hint(fmt.Errorf("reading repository %s/%s: %w", owner, repo, err), owner+"/"+repo)
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("repository %s/%s has no default branch (is it empty?)", owner, repo)
	}
	return r.DefaultBranch, nil
}

// ConflictError reports that the formula kept changing under us.
type ConflictError struct {
	Repository, Branch, Path string
	Attempts                 int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict: %s on %s@%s changed concurrently %d times; giving up (re-run to retry)", e.Path, e.Repository, e.Branch, e.Attempts)
}

// commit writes the formula to branch unless it is already identical. It
// returns whether a commit was made and whether the file existed before.
// A 409/422 from the optimistic lock is resolved by re-reading: identical
// content means someone (possibly a lost earlier attempt) already wrote it.
func (p *publisher) commit(ctx context.Context, owner, repo, branch string) (wrote, existed bool, err error) {
	full := owner + "/" + repo
	for attempt := 0; attempt < commitAttempts; attempt++ {
		cur, err := p.current(ctx, owner, repo, branch)
		if err != nil {
			return false, false, err
		}
		if attempt == 0 {
			existed = cur != nil
		}
		if cur != nil && bytes.Equal(cur.Content, p.content) {
			if attempt > 0 {
				p.logf("%s@%s already has the formula (written concurrently)", full, branch)
			}
			return attempt > 0, existed, nil
		}
		if p.in.DryRun {
			p.logf("would commit %s to %s@%s", p.p.Path, full, branch)
			return true, existed, nil
		}
		req := github.PutContentsRequest{Message: p.p.Message, Content: p.content, Branch: branch, Author: p.author(), Committer: p.author()}
		if cur != nil {
			req.SHA = cur.SHA
		}
		res, err := p.c.PutContents(ctx, owner, repo, p.p.Path, req)
		if err == nil {
			p.res.CommitSHA = res.CommitSHA
			p.logf("committed %s to %s@%s (%s)", p.p.Path, full, branch, res.CommitSHA)
			return true, existed, nil
		}
		switch github.StatusCode(err) {
		case http.StatusConflict, http.StatusUnprocessableEntity:
			p.logf("commit to %s@%s conflicted (%v); re-reading", full, branch, err)
			continue
		}
		return false, existed, hint(fmt.Errorf("committing %s to %s@%s: %w", p.p.Path, full, branch, err), full)
	}
	return false, existed, &ConflictError{Repository: full, Branch: branch, Path: p.p.Path, Attempts: commitAttempts}
}

func status(existed bool) string {
	if existed {
		return StatusUpdated
	}
	return StatusCreated
}

func (p *publisher) direct(ctx context.Context) error {
	owner, repo := p.p.Owner, p.p.Name
	branch := p.p.Branch
	if branch == "" {
		var err error
		if branch, err = p.defaultBranch(ctx, owner, repo); err != nil {
			return err
		}
	}
	p.res.Branch = branch
	wrote, existed, err := p.commit(ctx, owner, repo, branch)
	if err != nil {
		return err
	}
	switch {
	case !wrote:
		p.res.Status, p.res.NoOp = StatusUnchanged, true
		p.logf("%s/%s@%s is up to date", owner, repo, branch)
	case p.in.DryRun:
		p.res.Status = StatusPlanned
	default:
		p.res.Status = status(existed)
	}
	return nil
}

func (p *publisher) pullRequest(ctx context.Context) error {
	headOwner, headRepo, head := p.p.Owner, p.p.Name, p.p.Branch
	baseOwner, baseRepo := p.p.BaseOwner, p.p.BaseName
	if baseOwner == "" {
		baseOwner, baseRepo = headOwner, headRepo
	}
	cross := !strings.EqualFold(baseOwner, headOwner) || !strings.EqualFold(baseRepo, headRepo)
	baseFull, headFull := baseOwner+"/"+baseRepo, headOwner+"/"+headRepo
	base := p.p.BaseBranch
	if base == "" {
		var err error
		if base, err = p.defaultBranch(ctx, baseOwner, baseRepo); err != nil {
			return err
		}
	}
	if !cross && base == head {
		return fmt.Errorf("the head branch %q is the base branch; set repository.branch to a different branch", head)
	}

	// 1. Already on the base branch: nothing to propose.
	onBase, err := p.current(ctx, baseOwner, baseRepo, base)
	if err != nil {
		return err
	}
	if onBase != nil && bytes.Equal(onBase.Content, p.content) {
		p.res.Status, p.res.NoOp = StatusUnchanged, true
		p.logf("%s@%s is up to date", baseFull, base)
		return nil
	}
	existedOnBase := onBase != nil

	// 2. The head branch, created from the base branch's HEAD if missing.
	_, err = p.c.GetRef(ctx, headOwner, headRepo, "heads/"+head)
	branchExists := err == nil
	if err != nil && !errors.Is(err, github.ErrNotFound) {
		return hint(fmt.Errorf("reading branch %s@%s: %w", headFull, head, err), headFull)
	}
	planned := false
	if !branchExists {
		if p.in.DryRun {
			p.logf("would create branch %s@%s from %s@%s", headFull, head, baseFull, base)
			p.logf("would commit %s to %s@%s", p.p.Path, headFull, head)
			planned = true
		} else if err := p.createBranch(ctx, headOwner, headRepo, head, baseOwner, baseRepo, base); err != nil {
			return err
		}
	}

	// 3. The commit on the head branch.
	wrote := false
	if !planned {
		if wrote, _, err = p.commit(ctx, headOwner, headRepo, head); err != nil {
			return err
		}
		planned = p.in.DryRun && wrote
	}

	// 4. The pull request, reused when one is open for this head.
	headRef := head
	if cross {
		headRef = headOwner + ":" + head
	}
	pr, err := p.findPull(ctx, baseOwner, baseRepo, headOwner, head, base)
	if err != nil {
		return err
	}
	switch {
	case pr != nil:
		p.res.PullRequestURL = pr.HTMLURL
		p.logf("reusing open pull request %s", pr.HTMLURL)
	case p.in.DryRun:
		p.logf("would open a pull request %s -> %s@%s", headRef, baseFull, base)
		planned = true
	default:
		if pr, err = p.openPull(ctx, baseOwner, baseRepo, headOwner, head, headRef, base); err != nil {
			return err
		}
		p.res.PullRequestURL = pr.HTMLURL
		wrote = true
	}

	switch {
	case planned:
		p.res.Status = StatusPlanned
	case wrote || !branchExists:
		p.res.Status = status(existedOnBase)
	default:
		p.res.Status, p.res.NoOp = StatusUnchanged, true
		p.logf("branch %s@%s and its pull request are up to date", headFull, head)
	}
	return nil
}

// createBranch creates head from the base branch's HEAD. For a fork, the
// upstream commit is reachable from the fork because GitHub forks share
// objects. An existing branch (422, e.g. a concurrent run) is accepted.
func (p *publisher) createBranch(ctx context.Context, owner, repo, head, baseOwner, baseRepo, base string) error {
	ref, err := p.c.GetRef(ctx, baseOwner, baseRepo, "heads/"+base)
	if err != nil {
		return hint(fmt.Errorf("reading base branch %s/%s@%s: %w", baseOwner, baseRepo, base, err), baseOwner+"/"+baseRepo)
	}
	full := owner + "/" + repo
	if _, err := p.c.CreateRef(ctx, owner, repo, "refs/heads/"+head, ref.Object.SHA); err != nil {
		if github.StatusCode(err) == http.StatusUnprocessableEntity {
			if _, gerr := p.c.GetRef(ctx, owner, repo, "heads/"+head); gerr == nil {
				p.logf("branch %s@%s appeared concurrently; using it", full, head)
				return nil
			}
		}
		return hint(fmt.Errorf("creating branch %s@%s: %w", full, head, err), full)
	}
	p.logf("created branch %s@%s from %s/%s@%s", full, head, baseOwner, baseRepo, base)
	return nil
}

func (p *publisher) findPull(ctx context.Context, baseOwner, baseRepo, headOwner, head, base string) (*github.PullRequest, error) {
	prs, err := p.c.ListPulls(ctx, baseOwner, baseRepo, github.ListPullsOptions{Head: headOwner + ":" + head, Base: base, State: "open"})
	if err != nil {
		return nil, hint(fmt.Errorf("listing pull requests of %s/%s: %w", baseOwner, baseRepo, err), baseOwner+"/"+baseRepo)
	}
	for i := range prs {
		// Guard against servers ignoring the head filter.
		if prs[i].Head.Ref == head || prs[i].Head.Label == headOwner+":"+head {
			return &prs[i], nil
		}
	}
	return nil, nil
}

// openPull creates the pull request. CreatePull is never retried blindly: a
// 422 (e.g. "A pull request already exists") is answered by searching again.
func (p *publisher) openPull(ctx context.Context, baseOwner, baseRepo, headOwner, head, headRef, base string) (*github.PullRequest, error) {
	body := fmt.Sprintf("Update the %s formula to version %s.\n\nOpened by Hephaestus.", p.b.Name, p.in.Vars.Version)
	pr, err := p.c.CreatePull(ctx, baseOwner, baseRepo, github.NewPullRequest{
		Title: p.p.Message, Head: headRef, Base: base, Body: body, Draft: p.p.Draft,
	})
	if err == nil {
		p.logf("opened pull request %s", pr.HTMLURL)
		return pr, nil
	}
	if _, transient := p.c.RetryDelay(err, 0); github.StatusCode(err) == http.StatusUnprocessableEntity || transient {
		if found, ferr := p.findPull(ctx, baseOwner, baseRepo, headOwner, head, base); ferr == nil && found != nil {
			p.logf("pull request %s already exists; reusing it", found.HTMLURL)
			return found, nil
		}
	}
	return nil, hint(fmt.Errorf("opening a pull request on %s/%s: %w", baseOwner, baseRepo, err), baseOwner+"/"+baseRepo)
}
