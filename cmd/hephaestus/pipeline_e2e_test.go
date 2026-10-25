package hephaestus

import (
	"crypto/sha1" //nolint:gosec // G505: fake git object ids only
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/manifest"
)

// End-to-end tests of "hephaestus run" (build → archive → checksum →
// release → formula → tap) against one fake GitHub server that holds the
// release repository acme/tool and the tap acme/homebrew-tap.

const (
	e2eReleaseToken = "ghs_E2E_RELEASE_SENTINEL_5d1c0b7e9a"
	e2eTapToken     = "ghp_E2E_TAP_SENTINEL_83f2a6c4d0"
	e2eReleaseRepo  = "acme/tool"
	e2eTapRepo      = "acme/homebrew-tap"
	e2eCommit       = "0123456789abcdef0123456789abcdef01234567"
	e2eWeb          = "https://github.example/"
)

type e2eRequest struct {
	repo, method, path string
}

type e2eRelease struct {
	github.Release
	assets []*e2eAsset
	// createdDraft records that the create request asked for a draft.
	createdDraft bool
}

type e2eAsset struct {
	github.Asset
	data []byte
}

type e2ePull struct {
	number            int
	head, base, title string
}

// e2eGitHub is a minimal stateful GitHub API: releases and assets of
// acme/tool, contents, refs, compare and pulls of acme/homebrew-tap. Any
// other request fails the test.
type e2eGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu  sync.Mutex
	log []e2eRequest

	// acme/tool
	nextID   int64
	releases []*e2eRelease
	tags     map[string]string // tag -> commit
	// failPublish answers every publishing PATCH (draft: false) with 500.
	failPublish bool
	// failPulls answers every pull request creation with 403.
	failPulls bool

	// acme/homebrew-tap: each commit is a full snapshot of the files.
	branches map[string]string
	commits  map[string]map[string]string
	parents  map[string]string
	seq      int
	pulls    []*e2ePull
}

func newE2EGitHub(t *testing.T) *e2eGitHub {
	f := &e2eGitHub{t: t, nextID: 1000, tags: map[string]string{}, branches: map[string]string{}, commits: map[string]map[string]string{}, parents: map[string]string{}}
	root := f.sha()
	f.commits[root] = map[string]string{"README.md": "# acme tap\n"}
	f.branches["main"] = root
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *e2eGitHub) sha() string {
	f.seq++
	h := sha1.Sum([]byte("commit" + strconv.Itoa(f.seq))) //nolint:gosec // fake object id
	return hex.EncodeToString(h[:])
}

func (f *e2eGitHub) resetLog() {
	f.mu.Lock()
	f.log = nil
	f.mu.Unlock()
}

// requests returns the logged requests to repo; writes only the non-GETs.
func (f *e2eGitHub) requests(repo string, writes bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.log {
		if r.repo == repo && (!writes || r.method != http.MethodGet) {
			out = append(out, r.method+" "+r.path)
		}
	}
	return out
}

func (f *e2eGitHub) releaseList() []github.Release {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []github.Release
	for _, r := range f.releases {
		out = append(out, f.view(r))
	}
	return out
}

func (f *e2eGitHub) pullList() []e2ePull {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []e2ePull
	for _, p := range f.pulls {
		out = append(out, *p)
	}
	return out
}

// tapFile returns a file of the tap on branch.
func (f *e2eGitHub) tapFile(branch, path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.commits[f.branches[branch]][path]
	return c, ok
}

// seedRelease adds a published, immutable release whose assets are the
// given files (name -> content); its tag points to e2eCommit.
func (f *e2eGitHub) seedRelease(tag string, files map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel := &e2eRelease{Release: github.Release{ID: f.id(), TagName: tag, Name: tag, TargetCommitish: e2eCommit, Immutable: true}}
	for name, data := range files {
		f.addAsset(rel, name, data)
	}
	f.refresh(rel)
	f.releases = append(f.releases, rel)
	f.tags[tag] = e2eCommit
}

func (f *e2eGitHub) id() int64 { f.nextID++; return f.nextID }

func (f *e2eGitHub) addAsset(rel *e2eRelease, name string, data []byte) *e2eAsset {
	s := sha256.Sum256(data)
	d := "sha256:" + hex.EncodeToString(s[:])
	now := time.Now().UTC()
	a := &e2eAsset{Asset: github.Asset{ID: f.id(), Name: name, Size: int64(len(data)), State: github.AssetStateUploaded, ContentType: "application/octet-stream", Digest: &d, CreatedAt: now, UpdatedAt: now}, data: data}
	rel.assets = append(rel.assets, a)
	f.refresh(rel)
	return a
}

// refresh mimics GitHub: drafts live under "untagged-..." URLs.
func (f *e2eGitHub) refresh(rel *e2eRelease) {
	seg := rel.TagName
	if rel.Draft {
		seg = "untagged-" + strconv.FormatInt(rel.ID, 10)
	}
	rel.HTMLURL = e2eWeb + e2eReleaseRepo + "/releases/tag/" + seg
	for _, a := range rel.assets {
		a.BrowserDownloadURL = e2eWeb + e2eReleaseRepo + "/releases/download/" + seg + "/" + a.Name
		a.URL = f.srv.URL + "/repos/" + e2eReleaseRepo + "/releases/assets/" + strconv.FormatInt(a.ID, 10)
	}
}

func (f *e2eGitHub) view(r *e2eRelease) github.Release {
	out := r.Release
	out.Assets = f.assetViews(r)
	return out
}

func (f *e2eGitHub) assetViews(r *e2eRelease) []github.Asset {
	out := []github.Asset{}
	for _, a := range r.assets {
		out = append(out, a.Asset)
	}
	return out
}

func (f *e2eGitHub) release(id string) *e2eRelease {
	for _, r := range f.releases {
		if strconv.FormatInt(r.ID, 10) == id {
			return r
		}
	}
	return nil
}

func e2eReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func e2eNotFound(w http.ResponseWriter) { e2eReply(w, 404, map[string]any{"message": "Not Found"}) }

func (f *e2eGitHub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rest, ok := strings.CutPrefix(r.URL.Path, "/repos/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) < 2 {
		f.t.Errorf("fake GitHub: unhandled %s %s", r.Method, r.URL.Path)
		e2eNotFound(w)
		return
	}
	repo := parts[0] + "/" + parts[1]
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, e2eRequest{repo: repo, method: r.Method, path: r.URL.Path})

	// No published value may carry a token: release notes, an asset, a
	// formula, a commit message or a pull request.
	for _, tok := range []string{e2eReleaseToken, e2eTapToken} {
		if strings.Contains(string(body), tok) || strings.Contains(r.URL.RawQuery, tok) {
			f.t.Errorf("%s %s: request carries a token", r.Method, r.URL.Path)
		}
	}
	auth := r.Header.Get("Authorization")
	switch repo {
	case e2eReleaseRepo:
		if auth != "Bearer "+e2eReleaseToken {
			f.t.Errorf("%s %s: not the release token", r.Method, r.URL.Path)
			e2eReply(w, 401, map[string]any{"message": "Bad credentials"})
			return
		}
		f.serveRelease(w, r, parts[2:], body)
	case e2eTapRepo:
		if auth != "Bearer "+e2eTapToken {
			f.t.Errorf("%s %s: not the tap token", r.Method, r.URL.Path)
			e2eReply(w, 401, map[string]any{"message": "Bad credentials"})
			return
		}
		f.serveTap(w, r, parts[2:], body)
	default:
		f.t.Errorf("fake GitHub: unknown repository %s %s", r.Method, r.URL.Path)
		e2eNotFound(w)
	}
}

func (f *e2eGitHub) serveRelease(w http.ResponseWriter, r *http.Request, p []string, body []byte) {
	m := r.Method
	switch {
	case m == "GET" && len(p) == 4 && p[0] == "git" && p[1] == "ref" && p[2] == "tags":
		sha, ok := f.tags[p[3]]
		if !ok {
			e2eNotFound(w)
			return
		}
		e2eReply(w, 200, map[string]any{"ref": "refs/tags/" + p[3], "object": map[string]any{"sha": sha, "type": "commit"}})

	case m == "GET" && len(p) == 3 && p[0] == "releases" && p[1] == "tags":
		for _, rel := range f.releases {
			if rel.TagName == p[2] && !rel.Draft {
				e2eReply(w, 200, f.view(rel))
				return
			}
		}
		e2eNotFound(w)

	case m == "GET" && len(p) == 1 && p[0] == "releases":
		all := []github.Release{}
		for i := len(f.releases) - 1; i >= 0; i-- {
			all = append(all, f.view(f.releases[i]))
		}
		e2eReply(w, 200, all)

	case m == "POST" && len(p) == 1 && p[0] == "releases":
		var req github.ReleaseRequest
		if err := json.Unmarshal(body, &req); err != nil {
			e2eReply(w, 400, map[string]any{"message": "Problems parsing JSON"})
			return
		}
		for _, rel := range f.releases {
			if rel.TagName == req.TagName && !rel.Draft {
				e2eReply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "Release", "field": "tag_name", "code": "already_exists"}}})
				return
			}
		}
		rel := &e2eRelease{Release: github.Release{ID: f.id(), TagName: req.TagName, TargetCommitish: req.TargetCommitish}}
		rel.createdDraft = req.Draft != nil && *req.Draft
		f.apply(rel, req)
		f.releases = append(f.releases, rel)
		e2eReply(w, 201, f.view(rel))

	case m == "GET" && len(p) == 2 && p[0] == "releases" && p[1] != "assets":
		rel := f.release(p[1])
		if rel == nil {
			e2eNotFound(w)
			return
		}
		e2eReply(w, 200, f.view(rel))

	case m == "PATCH" && len(p) == 2 && p[0] == "releases":
		rel := f.release(p[1])
		if rel == nil {
			e2eNotFound(w)
			return
		}
		var req github.ReleaseRequest
		if err := json.Unmarshal(body, &req); err != nil {
			e2eReply(w, 400, map[string]any{"message": "Problems parsing JSON"})
			return
		}
		if rel.Immutable {
			e2eReply(w, 422, map[string]any{"message": "Cannot update an immutable release."})
			return
		}
		if f.failPublish && req.Draft != nil && !*req.Draft {
			w.Header().Set("Retry-After", "0")
			e2eReply(w, 500, map[string]any{"message": "Server Error"})
			return
		}
		f.apply(rel, req)
		e2eReply(w, 200, f.view(rel))

	case m == "GET" && len(p) == 3 && p[0] == "releases" && p[2] == "assets":
		rel := f.release(p[1])
		if rel == nil {
			e2eNotFound(w)
			return
		}
		e2eReply(w, 200, f.assetViews(rel))

	case m == "POST" && len(p) == 3 && p[0] == "releases" && p[2] == "assets":
		rel := f.release(p[1])
		if rel == nil {
			e2eNotFound(w)
			return
		}
		if rel.Immutable {
			e2eReply(w, 422, map[string]any{"message": "Cannot upload assets to an immutable release."})
			return
		}
		name := r.URL.Query().Get("name")
		for _, a := range rel.assets {
			if a.Name == name {
				e2eReply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "ReleaseAsset", "field": "name", "code": "already_exists"}}})
				return
			}
		}
		a := f.addAsset(rel, name, body)
		a.ContentType = r.Header.Get("Content-Type")
		e2eReply(w, 201, a.Asset)

	default:
		f.t.Errorf("fake GitHub: unhandled %s %s", m, r.URL.Path)
		e2eNotFound(w)
	}
}

// apply updates rel from req; publishing creates the tag (GitHub does so
// from target_commitish when the tag is missing).
func (f *e2eGitHub) apply(rel *e2eRelease, req github.ReleaseRequest) {
	if req.Name != nil {
		rel.Name = *req.Name
	}
	if req.Body != nil {
		rel.Body = *req.Body
	}
	if req.Draft != nil {
		rel.Draft = *req.Draft
	}
	if req.Prerelease != nil {
		rel.Prerelease = *req.Prerelease
	}
	if req.TargetCommitish != "" {
		rel.TargetCommitish = req.TargetCommitish
	}
	if !rel.Draft {
		if _, ok := f.tags[rel.TagName]; !ok {
			f.tags[rel.TagName] = rel.TargetCommitish
		}
	}
	f.refresh(rel)
}

func (f *e2eGitHub) serveTap(w http.ResponseWriter, r *http.Request, p []string, body []byte) {
	m := r.Method
	switch {
	case m == "GET" && len(p) == 0:
		e2eReply(w, 200, map[string]any{"name": "homebrew-tap", "full_name": e2eTapRepo, "default_branch": "main"})

	case m == "GET" && len(p) >= 1 && p[0] == "contents":
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		sha, ok := f.branches[ref]
		if !ok {
			e2eReply(w, 404, map[string]any{"message": "No commit found for the ref " + ref})
			return
		}
		files, path := f.commits[sha], strings.Join(p[1:], "/")
		if c, ok := files[path]; ok {
			e2eReply(w, 200, map[string]any{"type": "file", "path": path, "sha": e2eBlob(c), "size": len(c), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(c))})
			return
		}
		if path != "" {
			e2eNotFound(w)
			return
		}
		list := []any{}
		for name := range files {
			top, _, nested := strings.Cut(name, "/")
			typ := "file"
			if nested {
				typ = "dir"
			}
			list = append(list, map[string]any{"name": top, "path": top, "type": typ})
		}
		e2eReply(w, 200, list)

	case m == "PUT" && len(p) >= 2 && p[0] == "contents":
		var req struct {
			Message, Content, SHA, Branch string
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Message == "" {
			e2eReply(w, 422, map[string]any{"message": "Invalid request"})
			return
		}
		if req.Branch == "" {
			req.Branch = "main"
		}
		head, ok := f.branches[req.Branch]
		if !ok {
			e2eReply(w, 404, map[string]any{"message": "Branch " + req.Branch + " not found"})
			return
		}
		data, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			e2eReply(w, 422, map[string]any{"message": "content is not valid Base64"})
			return
		}
		path := strings.Join(p[1:], "/")
		cur, exists := f.commits[head][path]
		if (exists && req.SHA != e2eBlob(cur)) || (!exists && req.SHA != "") {
			e2eReply(w, 409, map[string]any{"message": path + " does not match " + req.SHA})
			return
		}
		snap := map[string]string{}
		for k, v := range f.commits[head] {
			snap[k] = v
		}
		snap[path] = string(data)
		sha := f.sha()
		f.commits[sha], f.parents[sha], f.branches[req.Branch] = snap, head, sha
		status := 201
		if exists {
			status = 200
		}
		e2eReply(w, status, map[string]any{"content": map[string]any{"sha": e2eBlob(string(data))}, "commit": map[string]any{"sha": sha, "html_url": e2eWeb + e2eTapRepo + "/commit/" + sha}})

	case m == "GET" && len(p) >= 4 && p[0] == "git" && p[1] == "ref" && p[2] == "heads":
		branch := strings.Join(p[3:], "/")
		sha, ok := f.branches[branch]
		if !ok {
			e2eNotFound(w)
			return
		}
		e2eReply(w, 200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}})

	case m == "POST" && len(p) == 2 && p[0] == "git" && p[1] == "refs":
		var req struct{ Ref, SHA string }
		_ = json.Unmarshal(body, &req)
		branch, ok := strings.CutPrefix(req.Ref, "refs/heads/")
		_, exists := f.branches[branch]
		_, known := f.commits[req.SHA]
		if !ok || exists || !known {
			e2eReply(w, 422, map[string]any{"message": "Reference already exists or object does not exist"})
			return
		}
		f.branches[branch] = req.SHA
		e2eReply(w, 201, map[string]any{"ref": req.Ref, "object": map[string]any{"sha": req.SHA, "type": "commit"}})

	case m == "GET" && len(p) >= 2 && p[0] == "compare":
		base, head, ok := strings.Cut(strings.Join(p[1:], "/"), "...")
		if o, b, cut := strings.Cut(head, ":"); cut && o == "acme" {
			head = b
		}
		baseSHA, ok1 := f.branches[base]
		headSHA, ok2 := f.branches[head]
		if !ok || !ok1 || !ok2 {
			e2eNotFound(w)
			return
		}
		f.compare(w, baseSHA, headSHA)

	case m == "GET" && len(p) == 1 && p[0] == "pulls":
		q := r.URL.Query()
		out := []any{}
		for _, pr := range f.pulls {
			if (q.Get("head") == "" || q.Get("head") == pr.head) && (q.Get("base") == "" || q.Get("base") == pr.base) {
				out = append(out, f.pullJSON(pr))
			}
		}
		e2eReply(w, 200, out)

	case m == "POST" && len(p) == 1 && p[0] == "pulls":
		if f.failPulls {
			e2eReply(w, 403, map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		var req github.NewPullRequest
		_ = json.Unmarshal(body, &req)
		label := req.Head
		if !strings.Contains(label, ":") {
			label = "acme:" + label
		}
		if _, ok := f.branches[strings.TrimPrefix(label, "acme:")]; !ok {
			e2eReply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "field": "head", "code": "invalid"}}})
			return
		}
		for _, pr := range f.pulls {
			if pr.head == label && pr.base == req.Base {
				e2eReply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "code": "custom", "message": "A pull request already exists for " + label + "."}}})
				return
			}
		}
		pr := &e2ePull{number: len(f.pulls) + 1, head: label, base: req.Base, title: req.Title}
		f.pulls = append(f.pulls, pr)
		e2eReply(w, 201, f.pullJSON(pr))

	default:
		// This includes PATCH git/refs/...: the tap stage must never move
		// (let alone force-update) a reference.
		f.t.Errorf("fake GitHub: unhandled %s %s", m, r.URL.Path)
		e2eNotFound(w)
	}
}

func (f *e2eGitHub) pullJSON(p *e2ePull) map[string]any {
	_, ref, _ := strings.Cut(p.head, ":")
	return map[string]any{
		"number": p.number, "state": "open", "title": p.title,
		"html_url": fmt.Sprintf("%s%s/pull/%d", e2eWeb, e2eTapRepo, p.number),
		"head":     map[string]any{"ref": ref, "label": p.head},
		"base":     map[string]any{"ref": p.base},
	}
}

// compare answers a three-dot comparison of two commits.
func (f *e2eGitHub) compare(w http.ResponseWriter, baseSHA, headSHA string) {
	ancestors := func(sha string) []string {
		var out []string
		for ; sha != ""; sha = f.parents[sha] {
			out = append(out, sha)
		}
		return out
	}
	baseList := ancestors(baseSHA)
	mb, ahead, behind := "", 0, 0
	for i, s := range ancestors(headSHA) {
		if j := slices.Index(baseList, s); j >= 0 {
			mb, ahead, behind = s, i, j
			break
		}
	}
	if mb == "" {
		e2eReply(w, 404, map[string]any{"message": "No common ancestor between the refs."})
		return
	}
	status := "diverged"
	switch {
	case baseSHA == headSHA:
		status = "identical"
	case mb == baseSHA:
		status = "ahead"
	case mb == headSHA:
		status = "behind"
	}
	files := []any{}
	from, to := f.commits[mb], f.commits[headSHA]
	for path, c := range to {
		if old, ok := from[path]; !ok {
			files = append(files, map[string]any{"filename": path, "status": "added"})
		} else if old != c {
			files = append(files, map[string]any{"filename": path, "status": "modified"})
		}
	}
	for path := range from {
		if _, ok := to[path]; !ok {
			files = append(files, map[string]any{"filename": path, "status": "removed"})
		}
	}
	e2eReply(w, 200, map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": ahead, "files": files})
}

func e2eBlob(content string) string {
	h := sha1.Sum(fmt.Appendf(nil, "blob %d\x00%s", len(content), content)) //nolint:gosec // git blob id
	return hex.EncodeToString(h[:])
}

// e2eEnv is one scenario: a project configured against a fresh fake.
type e2eEnv struct {
	t    *testing.T
	gh   *e2eGitHub
	root string
	cfg  string
}

// newE2E writes a project whose release goes to acme/tool and whose formula
// goes to acme/homebrew-tap through a pull request, both on one fake
// server. releaseExtra and brewExtra are appended to the flow mappings of
// release.github and brews[0].
func newE2E(t *testing.T, releaseExtra, brewExtra string) *e2eEnv {
	t.Helper()
	clearGitHubEnv(t)
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GITHUB_TOKEN", e2eReleaseToken)
	t.Setenv("HOMEBREW_TAP_GITHUB_TOKEN", e2eTapToken)
	gh := newE2EGitHub(t)
	extra := fmt.Sprintf(`release:
  github: {owner: acme, name: tool, api_url: %q, upload_url: %q%s}
brews:
  - repository: {owner: acme, name: homebrew-tap, pull_request: {enabled: true}}%s
`, gh.srv.URL, gh.srv.URL, releaseExtra, brewExtra)
	root := project(t, extra)
	return &e2eEnv{t: t, gh: gh, root: root, cfg: filepath.Join(root, "hephaestus.yaml")}
}

// run runs the CLI for tag; stages "" runs every configured stage. Neither
// token may appear in stdout, stderr, the step outputs or the summary.
func (e *e2eEnv) run(tag, stages string) (int, string) {
	e.t.Helper()
	args := []string{"run", "--config", e.cfg, "--tag", tag, "--commit", e2eCommit}
	if stages != "" {
		args = append(args, "--stages", stages)
	}
	code, out, errs := runCLI(e.t, args...)
	outputs, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	summary, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	for _, tok := range []string{e2eReleaseToken, e2eTapToken} {
		for what, s := range map[string]string{"stdout": out, "stderr": errs, "GITHUB_OUTPUT": string(outputs), "GITHUB_STEP_SUMMARY": string(summary)} {
			if strings.Contains(s, tok) {
				e.t.Errorf("%s of %v contains a token", what, args)
			}
		}
	}
	return code, out + errs
}

// mustRun runs and requires success.
func (e *e2eEnv) mustRun(tag, stages string) string {
	e.t.Helper()
	code, log := e.run(tag, stages)
	if code != 0 {
		e.t.Fatalf("run %s --stages %q: exit %d\n%s", tag, stages, code, log)
	}
	return log
}

// assets returns the manifest's release assets (archives and checksums)
// with their contents.
func (e *e2eEnv) assets() map[string][]byte {
	e.t.Helper()
	m, err := manifest.ReadFile(filepath.Join(e.root, "dist", "hephaestus-manifest.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, a := range m.Artifacts {
		if a.Kind != manifest.KindArchive && a.Kind != manifest.KindChecksum {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(a.Path)))
		if err != nil {
			e.t.Fatal(err)
		}
		out[a.Name] = data
	}
	if len(out) != 3 {
		e.t.Fatalf("expected 2 archives and 1 checksum file, got %d assets", len(out))
	}
	return out
}

// only returns the one release, failing when there is not exactly one.
func (e *e2eEnv) only() github.Release {
	e.t.Helper()
	rels := e.gh.releaseList()
	if len(rels) != 1 {
		e.t.Fatalf("%d releases, want 1: %+v", len(rels), rels)
	}
	return rels[0]
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// checkAssets requires the release to hold exactly want, byte for byte.
func checkAssets(t *testing.T, rel github.Release, gh *e2eGitHub, want map[string][]byte) {
	t.Helper()
	gh.mu.Lock()
	defer gh.mu.Unlock()
	var got *e2eRelease
	for _, r := range gh.releases {
		if r.ID == rel.ID {
			got = r
		}
	}
	if got == nil || len(got.assets) != len(want) {
		t.Fatalf("release assets: %+v, want %d", rel.Assets, len(want))
	}
	for _, a := range got.assets {
		if w, ok := want[a.Name]; !ok || string(w) != string(a.data) {
			t.Errorf("asset %s differs from the local file", a.Name)
		}
	}
}

func TestPipelineE2E(t *testing.T) {
	const relPrefix = "/repos/" + e2eReleaseRepo

	t.Run("stable", func(t *testing.T) {
		e := newE2E(t, "", "")
		e.mustRun("v1.0.0", "")
		rel := e.only()
		if rel.Draft || rel.Prerelease || rel.TagName != "v1.0.0" {
			t.Errorf("release: draft=%t prerelease=%t tag=%s", rel.Draft, rel.Prerelease, rel.TagName)
		}
		assets := e.assets()
		checkAssets(t, rel, e.gh, assets)
		writes := e.gh.requests(e2eReleaseRepo, true)
		// Created as a draft, every asset uploaded, then published once.
		if len(writes) != 1+len(assets)+1 || writes[0] != "POST "+relPrefix+"/releases" ||
			countPrefix(writes, "POST "+relPrefix+"/releases/") != len(assets) ||
			!strings.HasPrefix(writes[len(writes)-1], "PATCH "+relPrefix+"/releases/") {
			t.Errorf("release writes:\n%s", strings.Join(writes, "\n"))
		}
		e.gh.mu.Lock()
		createdDraft := e.gh.releases[0].createdDraft
		e.gh.mu.Unlock()
		if !createdDraft {
			t.Error("the release was not created as a draft")
		}

		// The formula points at the published assets and is proposed to
		// the tap through a pull request; main is untouched.
		local, err := os.ReadFile(filepath.Join(e.root, "dist", "homebrew", "tool.rb"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(local), e2eWeb+e2eReleaseRepo+"/releases/download/v1.0.0/tool_1.0.0_linux_amd64.tar.gz") {
			t.Errorf("formula lacks the published download URL:\n%s", local)
		}
		pulls := e.gh.pullList()
		if len(pulls) != 1 || pulls[0].base != "main" || pulls[0].head != "acme:tool-1.0.0" {
			t.Fatalf("pulls: %+v", pulls)
		}
		inTap, ok := e.gh.tapFile("tool-1.0.0", "tool.rb")
		if !ok || strings.ReplaceAll(inTap, "\r\n", "\n") != strings.ReplaceAll(string(local), "\r\n", "\n") {
			t.Errorf("formula on the head branch (found %t) differs from dist/homebrew/tool.rb", ok)
		}
		if _, ok := e.gh.tapFile("main", "tool.rb"); ok {
			t.Error("formula committed to the default branch in pull request mode")
		}

		// A second run writes nothing anywhere.
		e.gh.resetLog()
		e.mustRun("v1.0.0", "")
		if w := append(e.gh.requests(e2eReleaseRepo, true), e.gh.requests(e2eTapRepo, true)...); len(w) != 0 {
			t.Errorf("second run wrote:\n%s", strings.Join(w, "\n"))
		}
		if n := len(e.gh.pullList()); n != 1 {
			t.Errorf("%d pull requests after the second run", n)
		}
		e.only()
	})

	t.Run("prerelease", func(t *testing.T) {
		// skip_upload defaults to false (a prerelease would be published to
		// the tap); auto skips prereleases.
		e := newE2E(t, "", "\n    skip_upload: auto")
		log := e.mustRun("v1.0.0-rc.1", "")
		rel := e.only()
		if !rel.Prerelease || rel.Draft {
			t.Errorf("release: prerelease=%t draft=%t", rel.Prerelease, rel.Draft)
		}
		checkAssets(t, rel, e.gh, e.assets())
		if r := e.gh.requests(e2eTapRepo, false); len(r) != 0 {
			t.Errorf("tap requests for a prerelease:\n%s", strings.Join(r, "\n"))
		}
		if !strings.Contains(log, "skip_upload is auto and 1.0.0-rc.1 is a prerelease") {
			t.Errorf("no skip notice:\n%s", log)
		}
	})

	t.Run("draft", func(t *testing.T) {
		e := newE2E(t, ", draft: true", "")
		e.mustRun("v1.0.0", "build,archive,checksum,release")
		rel := e.only()
		if !rel.Draft {
			t.Error("draft: true published the release")
		}
		checkAssets(t, rel, e.gh, e.assets())
		if n := countPrefix(e.gh.requests(e2eReleaseRepo, true), "PATCH "); n != 0 {
			t.Errorf("%d PATCH requests for a draft release", n)
		}
		// A re-run keeps it a draft and writes nothing.
		e.gh.resetLog()
		e.mustRun("v1.0.0", "release")
		if w := e.gh.requests(e2eReleaseRepo, true); len(w) != 0 || !e.only().Draft {
			t.Errorf("re-run of a draft wrote:\n%s", strings.Join(w, "\n"))
		}
	})

	t.Run("immutable", func(t *testing.T) {
		for _, missing := range []bool{false, true} {
			t.Run(map[bool]string{false: "complete", true: "missing-asset"}[missing], func(t *testing.T) {
				e := newE2E(t, "", "")
				e.mustRun("v1.0.0", "build,archive,checksum")
				assets := e.assets()
				seed := map[string][]byte{}
				for name, data := range assets {
					seed[name] = data
				}
				if missing {
					delete(seed, "tool_1.0.0_linux_amd64.tar.gz")
				}
				e.gh.seedRelease("v1.0.0", seed)
				code, log := e.run("v1.0.0", "release")
				if w := e.gh.requests(e2eReleaseRepo, true); len(w) != 0 {
					t.Errorf("writes to an immutable release:\n%s", strings.Join(w, "\n"))
				}
				rel := e.only()
				checkAssets(t, rel, e.gh, seed)
				if missing {
					if code == 0 || !strings.Contains(log, "assets cannot be added") {
						t.Errorf("missing asset: exit %d\n%s", code, log)
					}
					return
				}
				if code != 0 || !strings.Contains(log, "up to date") {
					t.Errorf("identical immutable release: exit %d\n%s", code, log)
				}
			})
		}
	})

	t.Run("recovery", func(t *testing.T) {
		e := newE2E(t, "", "")
		e.gh.failPublish = true
		code, log := e.run("v1.0.0", "")
		if code == 0 || !strings.Contains(log, "publishing release") {
			t.Fatalf("first run: exit %d\n%s", code, log)
		}
		rel := e.only()
		if !rel.Draft {
			t.Fatal("release published although publishing failed")
		}
		checkAssets(t, rel, e.gh, e.assets())
		if n := countPrefix(e.gh.requests(e2eReleaseRepo, true), "PATCH "); n == 0 {
			t.Error("the first run never tried to publish")
		}
		if r := e.gh.requests(e2eTapRepo, false); len(r) != 0 {
			t.Errorf("tap touched after a failed release:\n%s", strings.Join(r, "\n"))
		}

		e.gh.mu.Lock()
		e.gh.failPublish = false
		e.gh.mu.Unlock()
		e.gh.resetLog()
		e.mustRun("v1.0.0", "")
		if w := e.gh.requests(e2eReleaseRepo, true); len(w) != 1 || w[0] != fmt.Sprintf("PATCH %s/releases/%d", relPrefix, rel.ID) {
			t.Errorf("recovery writes, want only the publishing PATCH:\n%s", strings.Join(w, "\n"))
		}
		after := e.only()
		if after.ID != rel.ID || after.Draft {
			t.Errorf("after recovery: id %d (was %d) draft=%t", after.ID, rel.ID, after.Draft)
		}
		checkAssets(t, after, e.gh, e.assets())
		if pulls := e.gh.pullList(); len(pulls) != 1 {
			t.Errorf("pulls after recovery: %+v", pulls)
		}
	})

	t.Run("tap-only", func(t *testing.T) {
		e := newE2E(t, "", "")
		e.mustRun("v1.0.0", "build,archive,checksum,release")
		if r := e.gh.requests(e2eTapRepo, false); len(r) != 0 {
			t.Fatalf("tap touched without the tap stage:\n%s", strings.Join(r, "\n"))
		}
		e.gh.resetLog()
		e.mustRun("v1.0.0", "formula,tap")
		if r := e.gh.requests(e2eReleaseRepo, true); len(r) != 0 {
			t.Errorf("formula,tap wrote to the release repository:\n%s", strings.Join(r, "\n"))
		}
		if pulls := e.gh.pullList(); len(pulls) != 1 || pulls[0].head != "acme:tool-1.0.0" {
			t.Errorf("pulls: %+v", pulls)
		}
		if countPrefix(e.gh.requests(e2eTapRepo, true), "PUT ") != 1 {
			t.Errorf("tap writes:\n%s", strings.Join(e.gh.requests(e2eTapRepo, true), "\n"))
		}
		e.only()
	})

	t.Run("tap-failure", func(t *testing.T) {
		// The release is published, then the tap pull request cannot be
		// created: a full re-run must not write to the release again and
		// must only finish the tap.
		e := newE2E(t, "", "")
		e.gh.failPulls = true
		code, log := e.run("v1.0.0", "")
		if code == 0 {
			t.Fatalf("first run succeeded although the pull request failed\n%s", log)
		}
		rel := e.only()
		if rel.Draft {
			t.Fatal("the release was not published before the tap stage")
		}
		checkAssets(t, rel, e.gh, e.assets())
		if pulls := e.gh.pullList(); len(pulls) != 0 {
			t.Fatalf("pulls after the failure: %+v", pulls)
		}

		e.gh.mu.Lock()
		e.gh.failPulls = false
		e.gh.mu.Unlock()
		e.gh.resetLog()
		e.mustRun("v1.0.0", "")
		if w := e.gh.requests(e2eReleaseRepo, true); len(w) != 0 {
			t.Errorf("re-run wrote to the published release:\n%s", strings.Join(w, "\n"))
		}
		if after := e.only(); after.ID != rel.ID || after.Draft {
			t.Errorf("after the re-run: id %d (was %d) draft=%t", after.ID, rel.ID, after.Draft)
		}
		if pulls := e.gh.pullList(); len(pulls) != 1 || pulls[0].head != "acme:tool-1.0.0" {
			t.Errorf("pulls after the re-run: %+v", pulls)
		}
	})

	t.Run("immutable-resumes-tap", func(t *testing.T) {
		// A complete immutable release whose notes differ from the
		// configuration: the full run verifies it read-only, leaves the
		// notes, and continues with the formula and the tap.
		e := newE2E(t, ", notes: \"changed notes\"", "")
		e.mustRun("v1.0.0", "build,archive,checksum")
		e.gh.seedRelease("v1.0.0", e.assets())
		e.gh.resetLog()
		log := e.mustRun("v1.0.0", "")
		if w := e.gh.requests(e2eReleaseRepo, true); len(w) != 0 {
			t.Errorf("writes to an immutable release:\n%s", strings.Join(w, "\n"))
		}
		if rel := e.only(); rel.Body != "" || !rel.Immutable {
			t.Errorf("immutable release changed: %+v", rel)
		}
		if !strings.Contains(log, "never edited") {
			t.Errorf("notes difference not reported:\n%s", log)
		}
		if pulls := e.gh.pullList(); len(pulls) != 1 {
			t.Errorf("pulls: %+v", pulls)
		}
	})
}
