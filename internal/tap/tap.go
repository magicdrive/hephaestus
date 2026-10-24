// Package tap publishes generated Homebrew formulae to tap repositories
// through the GitHub contents, refs and pulls APIs.
//
// Two modes are supported, resolved by homebrew.ResolvePublish:
//
//   - Pull request mode (the default). The formula is committed to a head
//     branch (default "{{ .ProjectName }}-{{ .Version }}", as GoReleaser) that
//     is created from the base branch when missing, and a pull request is
//     opened against the base (which may be another repository, e.g. the
//     upstream of a fork). An existing head branch is reused only when the
//     compare API shows that its changes since the merge base with the base
//     branch touch nothing but the formula file (a branch that merely fell
//     behind a base that moved on is fine); otherwise the run fails and
//     asks for the branch to be deleted or another one to be configured. An open pull request is reused only when both its head
//     (owner:branch) and its base branch match.
//   - Direct mode (repository.pull_request.enabled: false). The formula is
//     committed to repository.branch, or the default branch.
//
// Every write is preceded by a read: identical content is a no-op, so a
// re-run creates no commit and no pull request. Commits use the contents
// API with the current blob SHA as an optimistic lock; a conflict is
// re-read and retried a limited number of times. References are only ever
// created, never moved: there is no force update anywhere in this package.
//
// Downgrades: before writing, the version of the formula already in the tap
// (its class-level version statement, else a semantic version found as a
// path segment of its top-level or on_macos/on_linux url; see
// formulaVersion) is compared with the one being published. When the tap's
// is newer (e.g. an older tag's workflow was re-run) nothing is written and
// the result is skipped, with the reason and a warning ("Tap has a newer
// version"), unless brews[].allow_downgrade is set. A skip is not a failure,
// since patch releases of an older series legitimately meet a newer
// formula; the warning keeps a tap pinned to a bogus high version visible. Versions that are not semantic versions cannot be compared: the
// formula is then updated with a warning. This applies to both modes (in
// pull request mode the base branch is compared).
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
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
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
	StatusSkipped   = "skipped"   // skip_upload, or the tap has a newer version
	StatusFailed    = "failed"
)

// TokenEnv is the environment variable conventionally holding the tap token.
const TokenEnv = "HOMEBREW_TAP_GITHUB_TOKEN" //nolint:gosec // G101: environment variable name, not a credential

const (
	maxFormulaSize = 1 << 20 // the contents API returns inline content up to 1 MB
	commitAttempts = 3
	// maxCompareFiles is the number of files GitHub lists at most in a
	// comparison.
	maxCompareFiles = 300
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
	// Reason explains a skipped status.
	Reason string
	// Warnings are non-fatal problems worth surfacing (e.g. versions that
	// could not be compared, or a downgrade forced by allow_downgrade).
	Warnings []string
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
	// downOld and downNew are the versions behind a skipped downgrade.
	downOld, downNew string
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
		p.res.Status, p.res.NoOp, p.res.Reason = StatusSkipped, true, pub.SkipReason
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
	f, err := manifest.OpenConfined(p.in.Config.Root, art.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFormulaSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFormulaSize {
		return nil, fmt.Errorf("%s exceeds %d bytes", art.Path, maxFormulaSize)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != art.SHA256 || int64(len(data)) != art.Size {
		return nil, fmt.Errorf("%s does not match the manifest (sha256 %s, %d bytes; manifest %s, %d bytes): modified after it was generated?", art.Path, got, len(data), art.SHA256, art.Size)
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

// formulaDirs are the directories Homebrew looks for formulae in before the
// tap's root, in order (Tap#formula_dir). The first one that exists is the
// tap's only formula directory: formulae anywhere else are never found.
// Sharding into subdirectories works only under these directories; at the
// root, only top-level *.rb files are formulae.
var formulaDirs = []string{"Formula", "HomebrewFormula"}

// checkFormulaDir makes sure Homebrew will find the formula at p.p.Path in
// the tap as it is on ref, and that adding it does not hide the tap's other
// formulae: creating Formula/ in a tap that keeps formulae at its root
// makes Homebrew read only Formula/.
func (p *publisher) checkFormulaDir(ctx context.Context, owner, repo, ref string) error {
	full := owner + "/" + repo
	entries, err := p.c.ListDirectory(ctx, owner, repo, "", ref)
	if errors.Is(err, github.ErrNotFound) {
		entries, err = nil, nil // an empty repository
	}
	if err != nil {
		return hint(fmt.Errorf("listing %s@%s to find its formula directory: %w", full, ref, err), full)
	}
	dirs := map[string]bool{}
	var rootFormulae []string
	for _, e := range entries {
		switch {
		case e.Type == "dir":
			dirs[e.Name] = true
		case e.Type == "file" && strings.HasSuffix(e.Name, ".rb"):
			rootFormulae = append(rootFormulae, e.Name)
		}
	}
	active := ""
	for _, d := range formulaDirs {
		if dirs[d] {
			active = d
			break
		}
	}
	target := path.Dir(p.p.Path)
	if target == "." {
		target = ""
	}
	top, _, _ := strings.Cut(target, "/")
	switch {
	case active != "":
		if top == active {
			return nil
		}
		return fmt.Errorf("%s keeps its formulae in %s/, the only directory Homebrew reads once it exists, so %s would never be found: set directory to %q", full, active, p.p.Path, active)
	case target == "":
		return nil
	case slices.Contains(formulaDirs, top):
		if len(rootFormulae) == 0 {
			return nil // a new tap, or one without formulae yet
		}
		shown := rootFormulae
		if len(shown) > 5 {
			shown = append(shown[:5:5], "...")
		}
		return fmt.Errorf("%s keeps its formulae at the repository root (%s); creating %s/ would make Homebrew read only that directory and hide them: set directory to \"\" (the root) or move the existing formulae first", full, strings.Join(shown, ", "), top)
	default:
		return fmt.Errorf("%s keeps its formulae at the repository root, where Homebrew reads only top-level *.rb files, so %s would never be found: set directory to \"\" (the root)", full, p.p.Path)
	}
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
//
// With guard set, a formula on branch whose version is newer than the one
// being published is left alone and errDowngrade is returned.
func (p *publisher) commit(ctx context.Context, owner, repo, branch string, guard bool) (wrote, existed bool, err error) {
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
		if guard && cur != nil && p.downgrade(cur.Content) {
			return false, existed, errDowngrade
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
	if err := p.checkFormulaDir(ctx, owner, repo, branch); err != nil {
		return err
	}
	wrote, existed, err := p.commit(ctx, owner, repo, branch, true)
	if errors.Is(err, errDowngrade) {
		p.skipDowngrade()
		return nil
	}
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
	if err := p.checkFormulaDir(ctx, baseOwner, baseRepo, base); err != nil {
		return err
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
	if onBase != nil && p.downgrade(onBase.Content) {
		p.skipDowngrade()
		return nil
	}
	existedOnBase := onBase != nil

	// 2. The head branch, created from the base branch's HEAD if missing.
	_, err = p.c.GetRef(ctx, headOwner, headRepo, "heads/"+head)
	branchExists := err == nil
	if err != nil && !errors.Is(err, github.ErrNotFound) {
		return hint(fmt.Errorf("reading branch %s@%s: %w", headFull, head, err), headFull)
	}
	if branchExists {
		if err := p.verifyHeadBranch(ctx, baseOwner, baseRepo, base, headOwner, head, cross); err != nil {
			return err
		}
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
		if wrote, _, err = p.commit(ctx, headOwner, headRepo, head, false); err != nil {
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
		// Do not rely on the server-side filters: reuse only a pull request
		// whose head is exactly owner:branch and whose base is base.
		if strings.EqualFold(prs[i].Head.Label, headOwner+":"+head) && prs[i].Base.Ref == base {
			return &prs[i], nil
		}
	}
	return nil, nil
}

// verifyHeadBranch checks an existing head branch before it is reused: its
// changes since the merge base with the base branch (three-dot comparison)
// must touch nothing but the formula. Anything else may be someone else's
// work, or a leftover, that would be proposed under an automatic "formula
// update" title. A diverged branch is accepted on the same condition: the
// base merely moved on after the branch was created. A branch with no
// changes of its own (identical, or behind: e.g. its pull request was
// merged) is reused too; the formula is then committed to it as usual. A
// branch that cannot be verified (commits ahead but no file list, or a list
// at GitHub's 300-file limit, which may be truncated) is refused.
//
// Known limitation: the check and the later commit and pull request are
// separate API calls, so a push to the branch in between (by someone with
// write access to the tap) is not detected.
func (p *publisher) verifyHeadBranch(ctx context.Context, baseOwner, baseRepo, base, headOwner, head string, cross bool) error {
	spec := head
	if cross {
		spec = headOwner + ":" + head
	}
	baseFull := baseOwner + "/" + baseRepo
	where := fmt.Sprintf("branch %s/%s@%s", headOwner, p.p.Name, head)
	cmp, err := p.c.CompareCommits(ctx, baseOwner, baseRepo, base, spec)
	if err != nil {
		return hint(fmt.Errorf("comparing the existing %s with %s@%s: %w", where, baseFull, base, err), baseFull)
	}
	advice := "delete the branch or set repository.branch to another branch"
	switch cmp.Status {
	case github.CompareIdentical, github.CompareBehind, github.CompareAhead, github.CompareDiverged:
	default:
		return fmt.Errorf("the existing %s: unexpected comparison status %q with %s@%s", where, cmp.Status, baseFull, base)
	}
	if len(cmp.Files) >= maxCompareFiles {
		return fmt.Errorf("the existing %s changes %d or more files compared with %s@%s (GitHub truncates the list, so it cannot be verified); %s", where, maxCompareFiles, baseFull, base, advice)
	}
	if cmp.AheadBy > 0 && len(cmp.Files) == 0 {
		return fmt.Errorf("the existing %s has %d commits not on %s@%s but GitHub listed no changed files, so it cannot be verified; %s", where, cmp.AheadBy, baseFull, base, advice)
	}
	var other []string
	for _, f := range cmp.Files {
		for _, name := range []string{f.Filename, f.PreviousFilename} {
			if name != "" && name != p.p.Path {
				other = append(other, name)
			}
		}
	}
	if len(other) > 0 {
		if len(other) > 5 {
			other = append(other[:5], fmt.Sprintf("and %d more", len(other)-5))
		}
		return fmt.Errorf("the existing %s has changes other than %s (%s); %s", where, p.p.Path, strings.Join(other, ", "), advice)
	}
	return nil
}

// errDowngrade is returned by commit when the formula on the branch is newer.
var errDowngrade = errors.New("the tap has a newer formula")

var (
	versionStmtRe = regexp.MustCompile(`^version[ \t]+["']([^"'\n]+)["']`)
	urlStmtRe     = regexp.MustCompile(`^url[ \t]+["']([^"'\n]+)["']`)
	heredocRe     = regexp.MustCompile(`<<[~-]?(["']?)([A-Za-z_][A-Za-z0-9_]*)(["']?)`)
	doBlockRe     = regexp.MustCompile(`\bdo(?:[ \t]*\|[^|]*\|)?[ \t]*$`)
	semverRe      = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

// platformBlocks are the blocks whose url still describes the formula
// itself (unlike resource or test blocks).
var platformBlocks = map[string]bool{"on_macos": true, "on_linux": true, "on_arm": true, "on_intel": true}

// formulaVersion returns the version a formula declares. Only statements of
// the formula itself count, so a crafted line in a heredoc (caveats), a
// resource block or a method body cannot set it:
//
//   - a `version "x"` statement directly in the class body (indented by
//     exactly two spaces, outside any block and heredoc); else
//   - the first path segment that is a semantic version (the tag in a
//     GitHub release download URL) of the first url statement in the class
//     body or in on_macos/on_linux/on_arm/on_intel blocks.
//
// It returns "" when there is none. The parser follows the layout of
// generated and brew-style formulae; it does not evaluate Ruby.
func formulaVersion(content []byte) string {
	type block struct {
		indent int
		kind   string
	}
	var stack []block
	heredoc := ""
	firstURL := ""
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimLeft(line, " \t")
		if heredoc != "" {
			if trimmed == heredoc {
				heredoc = ""
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(trimmed)
		if m := heredocRe.FindStringSubmatch(trimmed); m != nil && m[1] == m[3] {
			heredoc = m[2]
		}
		if trimmed == "end" || strings.HasPrefix(trimmed, "end ") || strings.HasPrefix(trimmed, "end.") {
			if n := len(stack); n > 0 && stack[n-1].indent == indent {
				stack = stack[:n-1]
			}
			continue
		}
		if doBlockRe.MatchString(trimmed) {
			kind, _, _ := strings.Cut(trimmed, " ")
			stack = append(stack, block{indent, kind})
			continue
		}
		if len(stack) == 0 && indent == 2 && line[:2] == "  " {
			if m := versionStmtRe.FindStringSubmatch(trimmed); m != nil {
				return strings.TrimSpace(m[1])
			}
		}
		if firstURL == "" && indent >= 2 {
			m := urlStmtRe.FindStringSubmatch(trimmed)
			ok := m != nil && (len(stack) > 0 || indent == 2)
			for _, b := range stack {
				ok = ok && platformBlocks[b.kind]
			}
			if ok {
				firstURL = m[1]
			}
		}
	}
	if firstURL == "" {
		return ""
	}
	u, err := url.Parse(firstURL)
	if err != nil {
		return ""
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if semverRe.MatchString(seg) {
			return strings.TrimPrefix(seg, "v")
		}
	}
	return ""
}

type semver struct {
	core [3]uint64
	pre  []string
}

func parseSemver(s string) (semver, bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false
	}
	var v semver
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return semver{}, false
		}
		v.core[i] = n
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

// compareSemver orders a and b by semantic version precedence (build
// metadata is ignored).
func compareSemver(a, b semver) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			if a.core[i] < b.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xerr := strconv.ParseUint(x, 10, 64)
		yn, yerr := strconv.ParseUint(y, 10, 64)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func (p *publisher) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, w := range p.res.Warnings {
		if w == msg {
			return
		}
	}
	p.res.Warnings = append(p.res.Warnings, msg)
	p.logf("warning: %s", msg)
}

// downgrade reports whether publishing would replace existing (the formula
// in the tap) with an older version, in which case Reason is set. Versions
// that cannot be compared, and allow_downgrade, produce warnings instead.
func (p *publisher) downgrade(existing []byte) bool {
	oldV, newV := formulaVersion(existing), formulaVersion(p.content)
	if newV == "" {
		newV = p.in.Vars.Version
	}
	if oldV == "" || newV == "" || oldV == newV {
		return false
	}
	o, ok1 := parseSemver(oldV)
	n, ok2 := parseSemver(newV)
	if !ok1 || !ok2 {
		p.warn("cannot tell whether %s in the tap (version %q) is newer than version %q (not semantic versions); updating it", p.p.Path, oldV, newV)
		return false
	}
	if compareSemver(o, n) <= 0 {
		return false
	}
	if p.b.AllowDowngrade {
		p.warn("downgrading %s from %s to %s (allow_downgrade is set)", p.p.Path, oldV, newV)
		return false
	}
	p.downOld, p.downNew = oldV, newV
	p.res.Reason = fmt.Sprintf("the tap already has version %s of %s, newer than %s; not downgrading it (set brews[].allow_downgrade: true to force)", oldV, p.p.Path, newV)
	return true
}

// skipDowngrade records a skip because the tap is newer. The run still
// succeeds: re-running an older tag, or publishing a patch release of an
// older series (1.4.9 after 2.0.0), legitimately meets a newer formula. The
// skip is therefore surfaced as a warning (in the CLI summary) rather than
// as a failure; set brews[].allow_downgrade to overwrite.
func (p *publisher) skipDowngrade() {
	p.res.Status, p.res.NoOp = StatusSkipped, true
	p.warn("formula %s: Tap has a newer version (%s); not downgraded to %s (set brews[].allow_downgrade: true to force)", p.b.Name, p.downOld, p.downNew)
	p.logf("skipped: %s", p.res.Reason)
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
