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
	Draft     bool
}

func newFake(t *testing.T, tokens ...string) *fakeGitHub {
	f := &fakeGitHub{t: t, tokens: map[string]bool{}, repos: map[string]*fakeRepo{}, commits: map[string]map[string]string{}}
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

	case len(rest) >= 2 && rest[0] == "contents":
		path := strings.Join(rest[1:], "/")
		switch r.Method {
		case "GET":
			ref := r.URL.Query().Get("ref")
			if ref == "" {
				ref = repo.defaultBranch
			}
			sha, ok := repo.branches[ref]
			if !ok {
				reply(w, 404, map[string]any{"message": "No commit found for the ref " + ref})
				return
			}
			content, ok := f.commits[sha][path]
			if !ok {
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

	case len(rest) == 1 && rest[0] == "pulls" && r.Method == "GET":
		q := r.URL.Query()
		if q.Get("state") != "open" {
			f.t.Errorf("pulls listed with state %q", q.Get("state"))
		}
		out := []any{}
		for _, p := range f.pulls {
			if p.BaseRepo == full && (q.Get("head") == "" || q.Get("head") == p.HeadLabel) && (q.Get("base") == "" || q.Get("base") == p.Base) {
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
			if p.BaseRepo == full && p.HeadLabel == label {
				reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "code": "custom", "message": "A pull request already exists for " + label + "."}}})
				return
			}
		}
		p := &fakePull{Number: len(f.pulls) + 1, BaseRepo: full, HeadLabel: label, HeadRef: headRef, Base: req.Base, Title: req.Title, Draft: req.Draft}
		f.pulls = append(f.pulls, p)
		reply(w, 201, f.pullJSON(p))

	default:
		f.t.Errorf("fake GitHub: unhandled %s %s", r.Method, r.URL.Path)
		notFound(w)
	}
}

func (f *fakeGitHub) pullJSON(p *fakePull) map[string]any {
	return map[string]any{
		"number": p.Number, "state": "open", "title": p.Title,
		"html_url": fmt.Sprintf("https://github.example/%s/pull/%d", p.BaseRepo, p.Number),
		"head":     map[string]any{"ref": p.HeadRef, "label": p.HeadLabel},
		"base":     map[string]any{"ref": p.Base},
	}
}

// addPullLocked registers an open pull request (f.mu held).
func (f *fakeGitHub) addPullLocked(baseRepo, label, headRef, base string) {
	f.pulls = append(f.pulls, &fakePull{Number: len(f.pulls) + 1, BaseRepo: baseRepo, HeadLabel: label, HeadRef: headRef, Base: base})
}
