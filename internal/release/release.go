// Package release publishes manifest artifacts as a GitHub Release.
//
// The flow is designed for immutable releases and for safe re-runs:
//
//  1. Everything that can be checked locally is checked first: the tag, the
//     repository, every asset name, and every file's size and SHA-256
//     against the manifest (tampering after the build is detected).
//  2. The release is located by tag (published), then in the release list
//     (drafts). When there is none it is always created as a draft, so no
//     release becomes public, and therefore immutable, before its assets are
//     complete. A failed or timed-out create is followed by a re-read, never
//     by a blind second POST, so no duplicate release is created.
//  3. Assets are processed one by one. An identical asset is left alone; an
//     incomplete one ("starter") is deleted and uploaded again; a different
//     one is an error unless replace_existing_assets is set. Every upload is
//     verified against the size and digest GitHub reports.
//  4. Only when every asset is in place is the release published (unless
//     draft: true). A failure leaves the release a draft and returns an
//     error; the next run resumes where this one stopped.
//
// Tag handling: the tag must match the release's tag_name exactly. When the
// git tag does not exist yet, GitHub creates it when the release is
// published, from release.github.target_commitish, or from the default
// branch when that is unset. Pushing the tag before releasing is
// recommended; target_commitish is ignored by GitHub for an existing tag.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Limits imposed by GitHub.
const (
	// MaxAssetSize is the largest asset GitHub accepts (just under 2 GiB).
	MaxAssetSize = 2<<30 - 1
	// MaxAssets is the number of assets a release may hold.
	MaxAssets = 1000
	// MaxBodyLength is the longest release body, in characters.
	MaxBodyLength = 125000

	maxNotesFile   = 1 << 20
	createAttempts = 3
	uploadAttempts = 3
)

// Asset statuses.
const (
	StatusUploaded  = "uploaded"  // uploaded by this run
	StatusUnchanged = "unchanged" // already present with identical content
	StatusReplaced  = "replaced"  // a different asset was deleted and replaced
	StatusFailed    = "failed"
	StatusPlanned   = "planned" // dry run: would be uploaded or replaced
)

var (
	assetNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	repoNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	semverRe    = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

// Input configures Run.
type Input struct {
	Config   *config.Config
	Manifest *manifest.Manifest
	Vars     tmpl.Vars
	// Client may be nil in a dry run (no token).
	Client *github.Client
	// Owner and Repo default to release.github.owner and .name; callers fill
	// them from GITHUB_REPOSITORY when the configuration leaves them unset.
	Owner, Repo string
	Tag         string
	DryRun      bool
	Log         io.Writer
}

// Result describes what Run did, or would do in a dry run.
type Result struct {
	ReleaseID  int64
	ReleaseURL string
	// Created reports that the release was created (dry run: would be).
	Created bool
	// Published reports that the release is published (not a draft) when
	// Run returns.
	Published bool
	Assets    []AssetResult
	// NoOp reports that nothing was written (dry run: nothing would be).
	NoOp   bool
	DryRun bool
	// Actions lists the write operations performed, or planned in a dry run.
	Actions []string
}

// AssetResult is the outcome for one asset.
type AssetResult struct {
	Name   string
	Status string
	SHA256 string
	URL    string
	Err    error
}

// local is one asset to publish.
type local struct {
	idx         int // index in Manifest.Artifacts
	kind        manifest.Kind
	name        string
	path        string
	sha256      string
	size        int64
	contentType string
}

type runner struct {
	in         Input
	cfg        *config.GitHubRelease
	c          *github.Client
	log        io.Writer
	owner      string
	repo       string
	tag        string
	name       string
	body       string
	bodySet    bool
	prerelease bool
	assets     []*local
	res        *Result
	writes     int
}

// Run publishes the release described by in. On error the returned Result,
// when non-nil, still describes what happened to each asset.
func Run(ctx context.Context, in Input) (*Result, error) {
	r, err := prepare(in)
	if err != nil {
		return nil, err
	}
	return r.run(ctx)
}

// prepare validates everything that does not need the network.
func prepare(in Input) (*runner, error) {
	if in.Config == nil || in.Manifest == nil {
		return nil, errors.New("release: config and manifest are required")
	}
	cfg := in.Config.Release.GitHub
	if !cfg.IsEnabled() {
		return nil, errors.New("release: release.github is not configured or is disabled")
	}
	if in.Client == nil && !in.DryRun {
		return nil, errors.New("release: a GitHub client (token) is required unless --dry-run is set")
	}
	r := &runner{in: in, cfg: cfg, c: in.Client, log: in.Log, owner: in.Owner, repo: in.Repo, tag: in.Tag}
	if r.log == nil {
		r.log = io.Discard
	}
	if r.owner == "" {
		r.owner = cfg.Owner
	}
	if r.repo == "" {
		r.repo = cfg.Repo
	}
	var errs []error
	if err := validateTag(r.tag); err != nil {
		errs = append(errs, err)
	}
	if mt := in.Manifest.Tag; mt != "" && r.tag != "" && mt != r.tag {
		errs = append(errs, fmt.Errorf("tag %q does not match the manifest's tag %q", r.tag, mt))
	}
	if r.owner == "" || r.repo == "" {
		errs = append(errs, errors.New("repository owner and name are required (release.github.owner/name or GITHUB_REPOSITORY)"))
	} else {
		for _, s := range []string{r.owner, r.repo} {
			if !repoNameRe.MatchString(s) || s == "." || s == ".." {
				errs = append(errs, fmt.Errorf("invalid repository %q", r.owner+"/"+r.repo))
				break
			}
		}
	}
	if err := r.prepareText(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		errs = append(errs, r.prepareAssets()...)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	return r, nil
}

// validateTag applies the git ref-name rules that matter for a tag.
func validateTag(tag string) error {
	switch {
	case tag == "":
		return errors.New("tag is required")
	case len(tag) > 255:
		return errors.New("tag is too long")
	case strings.HasPrefix(tag, "-"), strings.HasPrefix(tag, "/"), strings.HasSuffix(tag, "/"),
		strings.HasSuffix(tag, "."), strings.HasSuffix(tag, ".lock"),
		strings.Contains(tag, ".."), strings.Contains(tag, "@{"), strings.Contains(tag, "//"), tag == "@":
		return fmt.Errorf("tag %q is not a valid git tag name", tag)
	}
	for _, r := range tag {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return fmt.Errorf("tag %q is not a valid git tag name", tag)
		}
	}
	return nil
}

// IsPrerelease reports whether tag is a semantic version with a
// pre-release part (v1.0.0-rc.1).
func IsPrerelease(tag string) bool {
	m := semverRe.FindStringSubmatch(tag)
	return m != nil && m[4] != ""
}

func (r *runner) prepareText() error {
	vars := r.in.Vars
	if vars.Tag == "" {
		vars.Tag = r.tag
	}
	r.name = r.tag
	if r.cfg.Name != "" {
		s, err := tmpl.Render(r.cfg.Name, vars)
		if err != nil {
			return fmt.Errorf("name_template: %w", err)
		}
		if s = strings.TrimSpace(s); s != "" {
			r.name = s
		}
	}
	switch {
	case r.cfg.NotesFile != "":
		p := filepath.Join(r.in.Config.Root, filepath.FromSlash(r.cfg.NotesFile))
		data, err := readFileLimited(p, maxNotesFile)
		if err != nil {
			return fmt.Errorf("notes_file: %w", err)
		}
		r.body, r.bodySet = string(data), true
	case r.cfg.Notes != "":
		s, err := tmpl.Render(r.cfg.Notes, vars)
		if err != nil {
			return fmt.Errorf("notes: %w", err)
		}
		r.body, r.bodySet = s, true
	}
	if !utf8.ValidString(r.body) {
		return errors.New("release notes are not valid UTF-8")
	}
	if n := utf8.RuneCountInString(r.body); n > MaxBodyLength {
		return fmt.Errorf("release notes are %d characters; GitHub allows %d", n, MaxBodyLength)
	}
	switch r.cfg.Prerelease {
	case "true":
		r.prerelease = true
	case "false":
		r.prerelease = false
	default: // "auto" or unset
		r.prerelease = IsPrerelease(r.tag)
	}
	return nil
}

func readFileLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", p, limit)
	}
	return data, nil
}

var kindOrder = map[manifest.Kind]int{manifest.KindBinary: 0, manifest.KindArchive: 1, manifest.KindChecksum: 2, manifest.KindFormula: 3}

func (r *runner) prepareAssets() []error {
	include := r.cfg.Include
	if len(include) == 0 {
		include = []string{string(manifest.KindArchive), string(manifest.KindChecksum)}
	}
	want := map[manifest.Kind]bool{}
	for _, k := range include {
		want[manifest.Kind(k)] = true
	}
	var errs []error
	seen := map[string]string{}
	for i, a := range r.in.Manifest.Artifacts {
		if !want[a.Kind] {
			continue
		}
		where := fmt.Sprintf("%s %q", a.Kind, a.Name)
		if !assetNameRe.MatchString(a.Name) {
			errs = append(errs, fmt.Errorf("%s: asset name must match %s (GitHub renames other names)", where, assetNameRe))
			continue
		}
		if prev, dup := seen[strings.ToLower(a.Name)]; dup {
			errs = append(errs, fmt.Errorf("%s: asset name collides with %q", where, prev))
			continue
		}
		seen[strings.ToLower(a.Name)] = a.Name
		l := &local{idx: i, kind: a.Kind, name: a.Name, path: filepath.Join(r.in.Config.Root, filepath.FromSlash(a.Path)), contentType: contentType(a.Name)}
		if err := verifyFile(l, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		r.assets = append(r.assets, l)
	}
	if len(errs) > 0 {
		return errs
	}
	if len(r.assets) == 0 {
		return []error{fmt.Errorf("the manifest has no artifacts of the included kinds (%s)", strings.Join(include, ", "))}
	}
	if len(r.assets) > MaxAssets {
		return []error{fmt.Errorf("%d assets exceed GitHub's limit of %d per release", len(r.assets), MaxAssets)}
	}
	sort.SliceStable(r.assets, func(i, j int) bool {
		a, b := r.assets[i], r.assets[j]
		if a.kind != b.kind {
			return kindOrder[a.kind] < kindOrder[b.kind]
		}
		return a.name < b.name
	})
	return nil
}

// verifyFile hashes the file and checks it against the manifest.
func verifyFile(l *local, a manifest.Artifact) error {
	if a.SHA256 == "" {
		return errors.New("the manifest records no sha256; refusing to upload an unverified file")
	}
	fi, err := os.Stat(l.path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", l.path)
	}
	if fi.Size() > MaxAssetSize {
		return fmt.Errorf("%s is %d bytes; GitHub accepts assets below 2 GiB", l.path, fi.Size())
	}
	sum, n, err := hashFile(l.path)
	if err != nil {
		return err
	}
	if n != a.Size {
		return fmt.Errorf("%s is %d bytes but the manifest records %d (modified after the build?)", l.path, n, a.Size)
	}
	if sum != a.SHA256 {
		return fmt.Errorf("%s has sha256 %s but the manifest records %s (modified after the build?)", l.path, sum, a.SHA256)
	}
	l.sha256, l.size = sum, n
	return nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(name, ".zip"):
		return "application/zip"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	}
	return "application/octet-stream"
}

func (r *runner) logf(format string, args ...any) {
	fmt.Fprintf(r.log, "release: "+format+"\n", args...)
}

func (r *runner) action(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	r.res.Actions = append(r.res.Actions, s)
	if r.in.DryRun {
		r.logf("[dry-run] would %s", s)
	} else {
		r.logf("%s", s)
	}
}

func (r *runner) run(ctx context.Context) (*Result, error) {
	r.res = &Result{DryRun: r.in.DryRun}
	repo := r.owner + "/" + r.repo
	var rel *github.Release
	if r.c != nil {
		var err error
		if rel, err = r.findRelease(ctx); err != nil {
			return nil, fmt.Errorf("release: looking up %s %s: %w", repo, r.tag, err)
		}
	}
	if rel == nil {
		r.res.Created = true
		if r.in.DryRun {
			return r.planNew(), nil
		}
		var err error
		if rel, err = r.createDraft(ctx); err != nil {
			return nil, fmt.Errorf("release: creating draft release %s for %s: %w", r.tag, repo, err)
		}
	} else {
		state := "draft"
		if !rel.Draft {
			state = "published"
			if rel.Immutable {
				state = "published, immutable"
			}
		}
		r.logf("found release %d for %s (%s)", rel.ID, r.tag, state)
	}
	if rel.TagName != r.tag {
		return nil, fmt.Errorf("release: release %d has tag %q, expected %q", rel.ID, rel.TagName, r.tag)
	}
	r.res.ReleaseID, r.res.ReleaseURL = rel.ID, rel.HTMLURL

	remote, err := r.c.ListReleaseAssets(ctx, r.owner, r.repo, rel.ID)
	if err != nil {
		return r.res, fmt.Errorf("release: listing assets of release %d: %w", rel.ID, err)
	}
	byName := map[string]github.Asset{}
	for _, a := range remote {
		byName[a.Name] = a
	}
	var failed []error
	for _, l := range r.assets {
		ar := r.processAsset(ctx, rel, byName, l)
		if ar.Err != nil {
			r.logf("asset %s: %v", l.name, ar.Err)
			failed = append(failed, fmt.Errorf("%s: %w", l.name, ar.Err))
		}
		r.res.Assets = append(r.res.Assets, ar)
	}
	if len(failed) > 0 {
		r.res.Published = !rel.Draft
		r.res.NoOp = r.writes == 0 && !r.res.Created
		what := "the release was not published"
		if !rel.Draft {
			what = "the published release was left as it was"
		}
		return r.res, fmt.Errorf("release %s: %d of %d assets failed; %s: %w", r.tag, len(failed), len(r.assets), what, errors.Join(failed...))
	}

	if rel, err = r.finalize(ctx, rel); err != nil {
		return r.res, err
	}
	r.res.ReleaseURL = rel.HTMLURL
	r.res.Published = !rel.Draft
	if err := r.recordURLs(ctx, rel, remote); err != nil {
		return r.res, err
	}
	if r.in.DryRun {
		r.res.NoOp = len(r.res.Actions) == 0
	} else {
		r.res.NoOp = r.writes == 0 && !r.res.Created
	}
	if r.res.NoOp {
		r.logf("release %s is up to date; nothing to do", r.tag)
	}
	return r.res, nil
}

// planNew describes a dry run against a release that does not exist.
func (r *runner) planNew() *Result {
	r.action("create draft release %s in %s/%s", r.tag, r.owner, r.repo)
	for _, l := range r.assets {
		r.action("upload %s (%d bytes, sha256:%s)", l.name, l.size, l.sha256)
		r.res.Assets = append(r.res.Assets, AssetResult{Name: l.name, Status: StatusPlanned, SHA256: l.sha256})
	}
	if !r.cfg.Draft {
		r.action("publish release %s", r.tag)
		r.res.Published = true
	}
	return r.res
}

// findRelease returns the release for the tag (published first, then
// drafts), or nil when there is none.
func (r *runner) findRelease(ctx context.Context) (*github.Release, error) {
	rel, err := r.c.GetReleaseByTag(ctx, r.owner, r.repo, r.tag)
	if err == nil {
		return rel, nil
	}
	if !errors.Is(err, github.ErrNotFound) {
		return nil, err
	}
	rel, err = r.c.FindReleaseByTag(ctx, r.owner, r.repo, r.tag)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	return rel, err
}

// createDraft creates the draft release. A create whose outcome is unknown
// (5xx, timeout, rate limit) or which reports that the release already
// exists is followed by a lookup, so a release created by a lost request is
// reused instead of duplicated.
func (r *runner) createDraft(ctx context.Context) (*github.Release, error) {
	req := github.ReleaseRequest{
		TagName:         r.tag,
		TargetCommitish: r.cfg.TargetCommitish,
		Name:            github.String(r.name),
		Draft:           github.Bool(true),
		Prerelease:      github.Bool(r.prerelease),
	}
	if r.bodySet {
		req.Body = github.String(r.body)
	}
	var lastErr error
	for attempt := 0; attempt < createAttempts; attempt++ {
		if attempt > 0 {
			wait, _ := r.c.RetryDelay(lastErr, attempt-1)
			if err := r.c.Wait(ctx, wait); err != nil {
				return nil, err
			}
			rel, err := r.findRelease(ctx)
			if err != nil {
				return nil, fmt.Errorf("%w (and the lookup after it failed: %v)", lastErr, err)
			}
			if rel != nil {
				r.logf("release %d for %s exists after a failed create; reusing it", rel.ID, r.tag)
				return rel, nil
			}
		}
		r.action("create draft release %s in %s/%s", r.tag, r.owner, r.repo)
		r.writes++
		rel, err := r.c.CreateRelease(ctx, r.owner, r.repo, req)
		if err == nil {
			r.logf("created draft release %d", rel.ID)
			return rel, nil
		}
		lastErr = err
		if github.IsAlreadyExists(err) {
			rel, ferr := r.findRelease(ctx)
			if ferr != nil {
				return nil, fmt.Errorf("%w (and the lookup after it failed: %v)", err, ferr)
			}
			if rel == nil {
				return nil, fmt.Errorf("%w: GitHub reports the release exists but it is not visible (does the token have push access?)", err)
			}
			r.logf("release %d for %s was created concurrently; reusing it", rel.ID, r.tag)
			return rel, nil
		}
		if _, ok := r.c.RetryDelay(err, attempt); !ok || ctx.Err() != nil {
			return nil, err
		}
		r.logf("create failed (%v); checking whether it took effect", err)
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", createAttempts, lastErr)
}

// processAsset brings one asset to the desired state.
func (r *runner) processAsset(ctx context.Context, rel *github.Release, byName map[string]github.Asset, l *local) AssetResult {
	ar := AssetResult{Name: l.name, SHA256: l.sha256}
	fail := func(err error) AssetResult {
		ar.Status, ar.Err = StatusFailed, err
		return ar
	}
	immutable := !rel.Draft && rel.Immutable
	status := StatusUploaded
	if e, ok := byName[l.name]; ok {
		if e.State == github.AssetStateUploaded {
			same, desc, err := r.compare(ctx, e, l)
			if err != nil {
				return fail(fmt.Errorf("comparing with the existing asset: %w", err))
			}
			if same {
				ar.Status, ar.URL = StatusUnchanged, e.BrowserDownloadURL
				return ar
			}
			switch {
			case immutable:
				return fail(fmt.Errorf("the immutable release already has a different %s (%s; local sha256:%s, %d bytes); a published immutable release cannot be changed, release a new version", l.name, desc, l.sha256, l.size))
			case !r.cfg.ReplaceExistingAssets:
				return fail(fmt.Errorf("the release already has a different %s (%s; local sha256:%s, %d bytes); set release.github.replace_existing_assets: true to replace it", l.name, desc, l.sha256, l.size))
			}
			status = StatusReplaced
			if r.in.DryRun {
				r.action("replace %s (%s) with sha256:%s", l.name, desc, l.sha256)
				ar.Status = StatusPlanned
				return ar
			}
			r.action("delete %s (%s) to replace it", l.name, desc)
		} else {
			if immutable {
				return fail(fmt.Errorf("the immutable release has an incomplete asset (state %q) that cannot be removed", e.State))
			}
			if r.in.DryRun {
				r.action("delete incomplete %s (state %q) and upload it again", l.name, e.State)
				ar.Status = StatusPlanned
				return ar
			}
			r.action("delete incomplete %s (state %q)", l.name, e.State)
		}
		if err := r.deleteAsset(ctx, e.ID); err != nil {
			return fail(fmt.Errorf("deleting the existing asset: %w", err))
		}
	} else if immutable {
		return fail(errors.New("the release is published and immutable, so assets cannot be added; release a new version"))
	}
	if r.in.DryRun {
		r.action("upload %s (%d bytes, sha256:%s)", l.name, l.size, l.sha256)
		ar.Status = StatusPlanned
		return ar
	}
	a, err := r.upload(ctx, rel.ID, l)
	if err != nil {
		return fail(err)
	}
	ar.Status, ar.URL = status, a.BrowserDownloadURL
	return ar
}

func (r *runner) deleteAsset(ctx context.Context, id int64) error {
	r.writes++
	err := r.c.DeleteReleaseAsset(ctx, r.owner, r.repo, id)
	if errors.Is(err, github.ErrNotFound) {
		return nil
	}
	return err
}

// compare reports whether remote asset e has l's content. It trusts the
// digest when GitHub provides one and otherwise downloads and hashes the
// asset (only when the sizes match).
func (r *runner) compare(ctx context.Context, e github.Asset, l *local) (bool, string, error) {
	desc := fmt.Sprintf("remote %d bytes", e.Size)
	if e.Size != l.size {
		return false, desc, nil
	}
	if e.Digest != nil && *e.Digest != "" {
		algo, sum, _ := strings.Cut(*e.Digest, ":")
		if algo == "sha256" {
			return strings.EqualFold(sum, l.sha256), "remote " + *e.Digest, nil
		}
	}
	h := sha256.New()
	if _, err := r.c.DownloadReleaseAsset(ctx, r.owner, r.repo, e.ID, h, l.size); err != nil {
		return false, desc, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return sum == l.sha256, "remote sha256:" + sum, nil
}

// upload uploads l, retrying a limited number of times. Before each retry
// the asset list is re-read: a lost success is accepted after verification
// and an incomplete leftover is deleted.
func (r *runner) upload(ctx context.Context, releaseID int64, l *local) (*github.Asset, error) {
	var lastErr error
	for attempt := 0; attempt < uploadAttempts; attempt++ {
		if attempt > 0 {
			wait, _ := r.c.RetryDelay(lastErr, attempt-1)
			if err := r.c.Wait(ctx, wait); err != nil {
				return nil, err
			}
			a, err := r.recover(ctx, releaseID, l)
			if err != nil {
				return nil, fmt.Errorf("%w (while recovering from: %v)", err, lastErr)
			}
			if a != nil {
				return a, nil
			}
		}
		r.action("upload %s (%d bytes)", l.name, l.size)
		a, err := r.uploadOnce(ctx, releaseID, l)
		if err == nil {
			return a, nil
		}
		lastErr = err
		var ve verifyError
		if errors.As(err, &ve) || ctx.Err() != nil {
			return nil, err
		}
		if _, ok := r.c.RetryDelay(err, attempt); !ok && !github.IsAlreadyExists(err) {
			return nil, err
		}
		r.logf("upload of %s failed (attempt %d/%d): %v", l.name, attempt+1, uploadAttempts, err)
	}
	return nil, fmt.Errorf("upload failed after %d attempts: %w", uploadAttempts, lastErr)
}

// recover inspects the asset list after a failed upload. It returns the
// asset when an earlier attempt completed with the right content, deletes
// an incomplete leftover, and fails if a different asset appeared.
func (r *runner) recover(ctx context.Context, releaseID int64, l *local) (*github.Asset, error) {
	list, err := r.c.ListReleaseAssets(ctx, r.owner, r.repo, releaseID)
	if err != nil {
		return nil, err
	}
	for _, e := range list {
		if e.Name != l.name {
			continue
		}
		if e.State != github.AssetStateUploaded {
			r.action("delete incomplete %s (state %q) left by the failed upload", l.name, e.State)
			if err := r.deleteAsset(ctx, e.ID); err != nil {
				return nil, fmt.Errorf("deleting incomplete asset: %w", err)
			}
			return nil, nil
		}
		same, desc, err := r.compare(ctx, e, l)
		if err != nil {
			return nil, err
		}
		if !same {
			return nil, fmt.Errorf("a different %s (%s) appeared on the release during the upload", l.name, desc)
		}
		r.logf("the failed upload of %s had completed; verified", l.name)
		return &e, nil
	}
	return nil, nil
}

// verifyError marks a completed upload whose result is wrong; it is not
// retried.
type verifyError struct{ msg string }

func (e verifyError) Error() string { return e.msg }

// uploadOnce streams the file while hashing it and verifies GitHub's view of
// the result.
func (r *runner) uploadOnce(ctx context.Context, releaseID int64, l *local) (*github.Asset, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, verifyError{err.Error()}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, verifyError{err.Error()}
	}
	if fi.Size() != l.size {
		return nil, verifyError{fmt.Sprintf("%s changed size since it was verified (%d, was %d)", l.path, fi.Size(), l.size)}
	}
	h := sha256.New()
	r.writes++
	a, err := r.c.UploadReleaseAsset(ctx, r.owner, r.repo, releaseID, l.name, io.TeeReader(f, h), l.size, l.contentType)
	if err != nil {
		return nil, err
	}
	var problem string
	switch sent := hex.EncodeToString(h.Sum(nil)); {
	case sent != l.sha256:
		problem = fmt.Sprintf("%s changed while it was uploaded (sent sha256:%s, expected %s)", l.path, sent, l.sha256)
	case a.Name != l.name:
		problem = fmt.Sprintf("GitHub stored the asset as %q", a.Name)
	case a.Size != l.size:
		problem = fmt.Sprintf("GitHub reports %d bytes, expected %d", a.Size, l.size)
	case a.State != "" && a.State != github.AssetStateUploaded:
		problem = fmt.Sprintf("GitHub reports state %q", a.State)
	case a.Digest != nil && *a.Digest != "" && !strings.EqualFold(*a.Digest, "sha256:"+l.sha256):
		problem = fmt.Sprintf("GitHub reports digest %s, expected sha256:%s", *a.Digest, l.sha256)
	}
	if problem == "" {
		return a, nil
	}
	// Remove the bad asset so the next run starts clean (best effort).
	if derr := r.deleteAsset(ctx, a.ID); derr != nil {
		problem += fmt.Sprintf("; deleting it failed: %v", derr)
	}
	return nil, verifyError{"upload verification failed: " + problem}
}

// finalize sets the title, notes and prerelease flag and publishes the
// release when configured, writing only what differs.
func (r *runner) finalize(ctx context.Context, rel *github.Release) (*github.Release, error) {
	var req github.ReleaseRequest
	var changes []string
	if rel.Name != r.name {
		req.Name = github.String(r.name)
		changes = append(changes, "name")
	}
	if r.bodySet && rel.Body != r.body {
		req.Body = github.String(r.body)
		changes = append(changes, "notes")
	}
	if rel.Prerelease != r.prerelease {
		req.Prerelease = github.Bool(r.prerelease)
		changes = append(changes, fmt.Sprintf("prerelease=%t", r.prerelease))
	}
	publish := rel.Draft && !r.cfg.Draft
	if publish {
		req.Draft = github.Bool(false)
		// make_latest is applied only when publishing: changing "latest" on
		// a re-run of an older tag would move it backwards.
		if r.cfg.MakeLatest != "" && !r.prerelease {
			req.MakeLatest = r.cfg.MakeLatest
		}
		if r.cfg.TargetCommitish != "" && rel.TargetCommitish != r.cfg.TargetCommitish {
			req.TargetCommitish = r.cfg.TargetCommitish
		}
	}
	if !rel.Draft && r.cfg.Draft {
		r.logf("release %s is already published; draft: true does not unpublish it", r.tag)
	}
	if !publish && len(changes) == 0 {
		return rel, nil
	}
	switch {
	case publish && len(changes) > 0:
		r.action("publish release %s (updating %s)", r.tag, strings.Join(changes, ", "))
	case publish:
		r.action("publish release %s", r.tag)
	default:
		r.action("update release %s (%s)", r.tag, strings.Join(changes, ", "))
	}
	if r.in.DryRun {
		cp := *rel
		cp.Draft = rel.Draft && !publish
		return &cp, nil
	}
	r.writes++
	updated, err := r.c.UpdateRelease(ctx, r.owner, r.repo, rel.ID, req)
	if err != nil {
		if publish {
			return rel, fmt.Errorf("release: all assets are uploaded but publishing release %d failed (re-run to retry): %w", rel.ID, err)
		}
		return rel, fmt.Errorf("release: updating release %d: %w", rel.ID, err)
	}
	if updated.TagName != r.tag {
		return updated, fmt.Errorf("release: release %d now has tag %q, expected %q", updated.ID, updated.TagName, r.tag)
	}
	if publish && updated.Draft {
		return updated, fmt.Errorf("release: release %d is still a draft after publishing", updated.ID)
	}
	return updated, nil
}

// recordURLs stores each asset's download URL in the manifest. Drafts are
// skipped: their URLs ("untagged-...") change when they are published.
func (r *runner) recordURLs(ctx context.Context, rel *github.Release, remote []github.Asset) error {
	if r.in.DryRun {
		return nil
	}
	if rel.Draft {
		r.logf("release %s is a draft; download URLs are recorded once it is published", r.tag)
		return nil
	}
	if r.writes > 0 || r.res.Created {
		var err error
		if remote, err = r.c.ListReleaseAssets(ctx, r.owner, r.repo, rel.ID); err != nil {
			return fmt.Errorf("release: listing assets after publishing: %w", err)
		}
	}
	byName := map[string]github.Asset{}
	for _, a := range remote {
		byName[a.Name] = a
	}
	for i, l := range r.assets {
		a, ok := byName[l.name]
		if !ok || a.State != github.AssetStateUploaded || a.BrowserDownloadURL == "" || a.Size != l.size {
			return fmt.Errorf("release: asset %s is missing or incomplete on release %d after publishing", l.name, rel.ID)
		}
		r.res.Assets[i].URL = a.BrowserDownloadURL
		r.in.Manifest.Artifacts[l.idx].DownloadURL = a.BrowserDownloadURL
	}
	return nil
}
