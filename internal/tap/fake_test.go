package tap

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
)

// fakeGitHub models repositories as branches pointing at commits, each
// commit being a full snapshot of files. Commits are global, as objects are
// shared between a repository and its forks on GitHub.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	tokens  map[string]bool
	repos   map[string]*fakeRepo
	commits map[string]map[string]string // sha -> path -> content
	parents map[string]string            // sha -> parent sha ("" for a root commit)
	seq     int
	pulls   []*fakePull
	log     []string
	// hook runs first (without mu held); returning true means handled.
	hook func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

type fakeRepo struct {
	defaultBranch string
	branches      map[string]string // branch -> commit sha
}

type fakePull struct {
	Number    int
	BaseRepo  string
	HeadLabel string
	HeadRef   string
	Base      string
	Title     string
	Body      string
	Draft     bool
	Closed    bool // merged
}

func newFake(t *testing.T, tokens ...string) *fakeGitHub {
	f := &fakeGitHub{t: t, tokens: map[string]bool{}, repos: map[string]*fakeRepo{}, commits: map[string]map[string]string{}, parents: map[string]string{}}
	for _, tok := range tokens {
		f.tokens[tok] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client(t *testing.T, token string) *github.Client {
	t.Helper()
	c, err := github.New(github.Options{
		Token: token, APIURL: f.srv.URL, UploadURL: f.srv.URL, HTTPClient: f.srv.Client(),
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeGitHub) nextSHA(prefix string) string {
	f.seq++
	h := sha1.Sum([]byte(prefix + strconv.Itoa(f.seq)))
	return hex.EncodeToString(h[:])
}

// addRepo creates a repository whose default branch holds files.
func (f *fakeGitHub) addRepo(full, defaultBranch string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := f.nextSHA("commit")
	snap := map[string]string{}
	for k, v := range files {
		snap[k] = v
	}
	f.commits[sha] = snap
	f.repos[full] = &fakeRepo{defaultBranch: defaultBranch, branches: map[string]string{defaultBranch: sha}}
}

// setFile commits content (or deletes with "") to a branch, as another
// actor would.
func (f *fakeGitHub) setFile(full, branch, path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setFileLocked(full, branch, path, content)
}

func (f *fakeGitHub) setFileLocked(full, branch, path, content string) string {
	repo := f.repos[full]
	snap := map[string]string{}
	for k, v := range f.commits[repo.branches[branch]] {
		snap[k] = v
	}
	if content == "" {
		delete(snap, path)
	} else {
		snap[path] = content
	}
	sha := f.nextSHA("commit")
	f.commits[sha] = snap
	f.parents[sha] = repo.branches[branch]
	repo.branches[branch] = sha
	return sha
}

func (f *fakeGitHub) file(full, branch, path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.repos[full]
	if repo == nil {
		return "", false
	}
	sha, ok := repo.branches[branch]
	if !ok {
		return "", false
	}
	c, ok := f.commits[sha][path]
	return c, ok
}

func (f *fakeGitHub) hasBranch(full, branch string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[full].branches[branch]
	return ok
}

func (f *fakeGitHub) pullList() []fakePull {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePull
	for _, p := range f.pulls {
		out = append(out, *p)
	}
	return out
}

func (f *fakeGitHub) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeGitHub) writes() []string {
	var out []string
	for _, l := range f.requests() {
		if !strings.HasPrefix(l, "GET ") {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeGitHub) count(prefix string) int {
	n := 0
	for _, l := range f.requests() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) resetLog() {
	f.mu.Lock()
	f.log = nil
	f.mu.Unlock()
}

// blobSHA mimics git's content addressing.
func blobSHA(content string) string {
	h := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
	return hex.EncodeToString(h[:])
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func notFound(w http.ResponseWriter) { reply(w, 404, map[string]any{"message": "Not Found"}) }

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	hook := f.hook
	tokOK := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()

	// Force-updating a reference must never happen; any ref update at all
	// is outside what the tap publisher may do.
	if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/git/refs/") {
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		f.t.Errorf("reference update attempted: %s %s force=%v", r.Method, r.URL.Path, b["force"])
		reply(w, 422, map[string]any{"message": "forbidden in tests"})
		return
	}
	if !tokOK {
		reply(w, 401, map[string]any{"message": "Bad credentials"})
		return
	}
	if hook != nil && hook(w, r, body) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.route(w, r, body)
}

func (f *fakeGitHub) route(w http.ResponseWriter, r *http.Request, body []byte) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if len(parts) < 2 {
		notFound(w)
		return
	}
	full := parts[0] + "/" + parts[1]
	repo := f.repos[full]
	if repo == nil {
		notFound(w)
		return
	}
	rest := parts[2:]
	switch {
	case r.Method == "GET" && len(rest) == 0:
		reply(w, 200, map[string]any{"full_name": full, "name": parts[1], "default_branch": repo.defaultBranch})

	case len(rest) >= 1 && rest[0] == "contents":
		path := strings.Join(rest[1:], "/")
		switch r.Method {
		case "GET":
			ref := r.URL.Query().Get("ref")
			if ref == "" {
				ref = repo.defaultBranch
			}
			sha, ok := repo.branches[ref]
			if _, isCommit := f.commits[ref]; !ok && isCommit {
				sha, ok = ref, true // a commit SHA (objects are shared with forks)
			}
			if !ok {
				reply(w, 404, map[string]any{"message": "No commit found for the ref " + ref})
				return
			}
			content, ok := f.commits[sha][path]
			if !ok {
				if list := dirListing(f.commits[sha], path); list != nil {
					reply(w, 200, list)
					return
				}
				notFound(w)
				return
			}
			enc := base64.StdEncoding.EncodeToString([]byte(content))
			// GitHub wraps base64 at 60 characters.
			var wrapped strings.Builder
			for len(enc) > 60 {
				wrapped.WriteString(enc[:60] + "\n")
				enc = enc[60:]
			}
			wrapped.WriteString(enc + "\n")
			reply(w, 200, map[string]any{"type": "file", "path": path, "sha": blobSHA(content), "size": len(content), "encoding": "base64", "content": wrapped.String()})
		case "PUT":
			var req struct {
				Message, Content, SHA, Branch string
				Author, Committer             *github.CommitAuthor
			}
			if err := json.Unmarshal(body, &req); err != nil || req.Message == "" {
				reply(w, 422, map[string]any{"message": "Invalid request"})
				return
			}
			if req.Branch == "" {
				req.Branch = repo.defaultBranch
			}
			if _, ok := repo.branches[req.Branch]; !ok {
				reply(w, 404, map[string]any{"message": "Branch " + req.Branch + " not found"})
				return
			}
			data, err := base64.StdEncoding.DecodeString(req.Content)
			if err != nil {
				reply(w, 422, map[string]any{"message": "content is not valid Base64"})
				return
			}
			cur, exists := f.commits[repo.branches[req.Branch]][path]
			switch {
			case exists && req.SHA == "":
				reply(w, 422, map[string]any{"message": "Invalid request.\n\n\"sha\" wasn't supplied."})
				return
			case exists && req.SHA != blobSHA(cur), !exists && req.SHA != "":
				reply(w, 409, map[string]any{"message": path + " does not match " + req.SHA})
				return
			}
			commit := f.setFileLocked(full, req.Branch, path, string(data))
			status := 201
			if exists {
				status = 200
			}
			reply(w, status, map[string]any{"content": map[string]any{"sha": blobSHA(string(data))}, "commit": map[string]any{"sha": commit, "html_url": "https://github.example/" + full + "/commit/" + commit}})
		default:
			notFound(w)
		}

	case r.Method == "GET" && len(rest) >= 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads":
		branch := strings.Join(rest[3:], "/")
		sha, ok := repo.branches[branch]
		if !ok {
			notFound(w)
			return
		}
		reply(w, 200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}})

	case r.Method == "POST" && len(rest) == 2 && rest[0] == "git" && rest[1] == "refs":
		var req struct{ Ref, SHA string }
		_ = json.Unmarshal(body, &req)
		branch, ok := strings.CutPrefix(req.Ref, "refs/heads/")
		if !ok {
			reply(w, 422, map[string]any{"message": "Reference name must start with refs/heads/ in tests"})
			return
		}
		if _, exists := repo.branches[branch]; exists {
			reply(w, 422, map[string]any{"message": "Reference already exists"})
			return
		}
		if _, ok := f.commits[req.SHA]; !ok {
			reply(w, 422, map[string]any{"message": "Object does not exist"})
			return
		}
		repo.branches[branch] = req.SHA
		reply(w, 201, map[string]any{"ref": req.Ref, "object": map[string]any{"sha": req.SHA, "type": "commit"}})

	case r.Method == "GET" && len(rest) >= 2 && rest[0] == "compare":
		baseRef, headSpec, ok := strings.Cut(strings.Join(rest[1:], "/"), "...")
		if !ok {
			notFound(w)
			return
		}
		headRepo := repo
		if o, b, ok := strings.Cut(headSpec, ":"); ok {
			headRepo, headSpec = f.repos[o+"/"+parts[1]], b
		}
		baseSHA, ok1 := repo.branches[baseRef]
		headSHA := ""
		if headRepo != nil {
			headSHA = headRepo.branches[headSpec]
		}
		if !ok1 || headSHA == "" {
			notFound(w)
			return
		}
		f.compareLocked(w, baseSHA, headSHA)

	case len(rest) == 1 && rest[0] == "pulls" && r.Method == "GET":
		q := r.URL.Query()
		if q.Get("state") != "open" {
			f.t.Errorf("pulls listed with state %q", q.Get("state"))
		}
		out := []any{}
		for _, p := range f.pulls {
			if !p.Closed && p.BaseRepo == full && (q.Get("head") == "" || q.Get("head") == p.HeadLabel) && (q.Get("base") == "" || q.Get("base") == p.Base) {
				out = append(out, f.pullJSON(p))
			}
		}
		reply(w, 200, out)

	case len(rest) == 1 && rest[0] == "pulls" && r.Method == "POST":
		var req github.NewPullRequest
		_ = json.Unmarshal(body, &req)
		label, headRef := req.Head, req.Head
		if o, b, ok := strings.Cut(req.Head, ":"); ok {
			headRef = b
			if hr := f.repos[o+"/"+parts[1]]; hr == nil || hr.branches[b] == "" {
				reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "field": "head", "code": "invalid"}}})
				return
			}
		} else {
			label = parts[0] + ":" + req.Head
			if repo.branches[req.Head] == "" {
				reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "field": "head", "code": "invalid"}}})
				return
			}
		}
		for _, p := range f.pulls {
			if !p.Closed && p.BaseRepo == full && p.HeadLabel == label && p.Base == req.Base {
				reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "code": "custom", "message": "A pull request already exists for " + label + "."}}})
				return
			}
		}
		p := &fakePull{Number: len(f.pulls) + 1, BaseRepo: full, HeadLabel: label, HeadRef: headRef, Base: req.Base, Title: req.Title, Body: req.Body, Draft: req.Draft}
		f.pulls = append(f.pulls, p)
		reply(w, 201, f.pullJSON(p))

	default:
		f.t.Errorf("fake GitHub: unhandled %s %s", r.Method, r.URL.Path)
		notFound(w)
	}
}

// dirListing answers a contents request for a directory ("" is the root),
// or returns nil when dir holds nothing.
func dirListing(files map[string]string, dir string) []any {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]bool{}
	var out []any
	for p := range files {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		typ := "file"
		if nested {
			typ = "dir"
		}
		out = append(out, map[string]any{"name": name, "path": prefix + name, "type": typ})
	}
	return out
}

func (f *fakeGitHub) pullJSON(p *fakePull) map[string]any {
	// The head commit is the head branch's current commit, in the base
	// repository or the fork the label names.
	headSHA := ""
	if owner, _, ok := strings.Cut(p.HeadLabel, ":"); ok {
		_, name, _ := strings.Cut(p.BaseRepo, "/")
		if hr := f.repos[owner+"/"+name]; hr != nil {
			headSHA = hr.branches[p.HeadRef]
		}
	}
	return map[string]any{
		"number": p.Number, "state": "open", "title": p.Title, "body": p.Body,
		"html_url": fmt.Sprintf("https://github.example/%s/pull/%d", p.BaseRepo, p.Number),
		"head":     map[string]any{"ref": p.HeadRef, "label": p.HeadLabel, "sha": headSHA},
		"base":     map[string]any{"ref": p.Base},
	}
}

// addPullLocked registers an open pull request (f.mu held).
func (f *fakeGitHub) addPullLocked(baseRepo, label, headRef, base string) {
	f.pulls = append(f.pulls, &fakePull{Number: len(f.pulls) + 1, BaseRepo: baseRepo, HeadLabel: label, HeadRef: headRef, Base: base})
}

// compareLocked answers a three-dot comparison (f.mu held).
func (f *fakeGitHub) compareLocked(w http.ResponseWriter, baseSHA, headSHA string) {
	ancestors := func(sha string) []string {
		var out []string
		for ; sha != ""; sha = f.parents[sha] {
			out = append(out, sha)
		}
		return out
	}
	baseList, headList := ancestors(baseSHA), ancestors(headSHA)
	baseIdx := map[string]int{}
	for i, s := range baseList {
		baseIdx[s] = i
	}
	mb, ahead, behind := "", 0, 0
	for i, s := range headList {
		if j, ok := baseIdx[s]; ok {
			mb, ahead, behind = s, i, j
			break
		}
	}
	if mb == "" {
		reply(w, 404, map[string]any{"message": "No common ancestor between the refs."})
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
	reply(w, 200, map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": ahead, "files": files})
}

// openRivalPull creates branch from base's HEAD with content at path and
// opens a pull request for it, as another release would.
func (f *fakeGitHub) openRivalPull(full, base, branch, path, content string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.repos[full]
	repo.branches[branch] = repo.branches[base]
	f.setFileLocked(full, branch, path, content)
	owner, _, _ := strings.Cut(full, "/")
	f.addPullLocked(full, owner+":"+branch, branch, base)
	return len(f.pulls)
}

// mergeBranch fast-forwards (or overwrites) base with branch's commit, as
// merging a pull request does; the pull request is closed.
func (f *fakeGitHub) mergeBranch(full, base, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.repos[full]
	snap := map[string]string{}
	for k, v := range f.commits[repo.branches[base]] {
		snap[k] = v
	}
	for k, v := range f.commits[repo.branches[branch]] {
		snap[k] = v
	}
	sha := f.nextSHA("merge")
	f.commits[sha] = snap
	f.parents[sha] = repo.branches[base]
	repo.branches[base] = sha
	owner, _, _ := strings.Cut(full, "/")
	for _, p := range f.pulls {
		if p.BaseRepo == full && p.HeadLabel == owner+":"+branch {
			p.Closed = true
		}
	}
}
