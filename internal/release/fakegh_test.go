package release

import (
	"crypto/sha256"
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

const (
	fakeToken = "ghs_FAKETOKEN_do_not_leak_0123456789"
	fakeOwner = "acme"
	fakeRepo  = "tool"
	pageSize  = 2 // small, so pagination is exercised
	webBase   = "https://github.example/" + fakeOwner + "/" + fakeRepo
)

// fakeGitHub is a stateful in-memory GitHub releases API. API and upload
// endpoints share one server; asset downloads redirect to a second server
// standing in for object storage.
type fakeGitHub struct {
	t       *testing.T
	api     *httptest.Server
	storage *httptest.Server

	mu       sync.Mutex
	nextID   int64
	releases []*fakeRelease
	// digests makes uploads record a sha256 digest (GitHub since 2025-06).
	digests bool
	// immutable makes releases immutable when published.
	immutable bool
	// tags are the git tags that exist (GET git/ref/tags/<tag>).
	tags map[string]bool
	// hook runs before the default handling; returning true means it wrote
	// the response. It is called without f.mu held.
	hook        func(w http.ResponseWriter, r *http.Request, body []byte) bool
	log         []string
	storageAuth []string
	sleeps      []time.Duration
}

type fakeRelease struct {
	github.Release
	assets []*fakeAsset
}

type fakeAsset struct {
	github.Asset
	data []byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, nextID: 100, digests: true}
	f.storage = httptest.NewServer(http.HandlerFunc(f.serveStorage))
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	t.Cleanup(func() { f.api.Close(); f.storage.Close() })
	return f
}

func (f *fakeGitHub) client(t *testing.T) *github.Client {
	t.Helper()
	c, err := github.New(github.Options{
		Token: fakeToken, APIURL: f.api.URL, UploadURL: f.api.URL, HTTPClient: f.api.Client(),
		Sleep: func(d time.Duration) {
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			f.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// requests returns the logged "METHOD /path" lines.
func (f *fakeGitHub) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// writes returns the logged requests that are not GETs.
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
	f.log, f.sleeps, f.storageAuth = nil, nil, nil
	f.mu.Unlock()
}

// addRelease seeds a release with assets (name -> content).
func (f *fakeGitHub) addRelease(tag string, draft, immutable bool, assets map[string]string) *fakeRelease {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel := &fakeRelease{Release: github.Release{ID: f.id(), TagName: tag, Name: tag, Draft: draft, Immutable: immutable && !draft}}
	f.refreshURLs(rel)
	f.releases = append(f.releases, rel)
	for name, content := range assets {
		f.addAssetLocked(rel, name, []byte(content), github.AssetStateUploaded)
	}
	return rel
}

func (f *fakeGitHub) addAssetLocked(rel *fakeRelease, name string, data []byte, state string) *fakeAsset {
	a := &fakeAsset{Asset: github.Asset{ID: f.id(), Name: name, Size: int64(len(data)), State: state, ContentType: "application/octet-stream"}, data: data}
	if f.digests && state == github.AssetStateUploaded {
		s := sha256.Sum256(data)
		d := "sha256:" + hex.EncodeToString(s[:])
		a.Digest = &d
	}
	rel.assets = append(rel.assets, a)
	f.refreshURLs(rel)
	return a
}

func (f *fakeGitHub) id() int64 { f.nextID++; return f.nextID }

// refreshURLs mimics GitHub: drafts live under "untagged-..." URLs.
func (f *fakeGitHub) refreshURLs(rel *fakeRelease) {
	seg := rel.TagName
	if rel.Draft {
		seg = "untagged-" + strconv.FormatInt(rel.ID, 10)
	}
	rel.HTMLURL = webBase + "/releases/tag/" + seg
	for _, a := range rel.assets {
		a.BrowserDownloadURL = webBase + "/releases/download/" + seg + "/" + a.Name
		a.URL = f.api.URL + "/repos/" + fakeOwner + "/" + fakeRepo + "/releases/assets/" + strconv.FormatInt(a.ID, 10)
	}
}

func (f *fakeGitHub) release(id int64) *fakeRelease {
	for _, r := range f.releases {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f *fakeGitHub) byTag(tag string) []*fakeRelease {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeRelease
	for _, r := range f.releases {
		if r.TagName == tag {
			out = append(out, r)
		}
	}
	return out
}

// byTagLocked returns the first release for tag; f.mu must be held.
func (f *fakeGitHub) byTagLocked(tag string) *fakeRelease {
	for _, r := range f.releases {
		if r.TagName == tag {
			return r
		}
	}
	return nil
}

func (f *fakeGitHub) asset(id int64) (*fakeRelease, int) {
	for _, r := range f.releases {
		for i, a := range r.assets {
			if a.ID == id {
				return r, i
			}
		}
	}
	return nil, -1
}

func (f *fakeGitHub) view(r *fakeRelease) github.Release {
	out := r.Release
	out.Assets = f.assetViews(r)
	return out
}

func (f *fakeGitHub) assetViews(r *fakeRelease) []github.Asset {
	out := []github.Asset{}
	for _, a := range r.assets {
		out = append(out, a.Asset)
	}
	return out
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func validationFailed(w http.ResponseWriter, resource, field, code string) {
	reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": resource, "field": field, "code": code}}})
}

func (f *fakeGitHub) serveStorage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storageAuth = append(f.storageAuth, r.Header.Get("Authorization"))
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/blob/"), 10, 64)
	rel, i := f.asset(id)
	if rel == nil {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(rel.assets[i].data)
}

func (f *fakeGitHub) serveAPI(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.ContentLength >= 0 && int64(len(body)) != r.ContentLength {
		f.t.Errorf("%s %s: body %d bytes, Content-Length %d", r.Method, r.URL.Path, len(body), r.ContentLength)
	}
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	hook := f.hook
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
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
	if t, ok := strings.CutPrefix(r.URL.Path, "/repos/"+fakeOwner+"/"+fakeRepo+"/git/ref/tags/"); ok && r.Method == "GET" {
		if !f.tags[t] {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		reply(w, 200, map[string]any{"ref": "refs/tags/" + t, "object": map[string]any{"sha": strings.Repeat("a", 40), "type": "commit"}})
		return
	}
	prefix := "/repos/" + fakeOwner + "/" + fakeRepo + "/releases"
	p, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		reply(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "tags":
		for _, rel := range f.releases {
			if rel.TagName == parts[1] && !rel.Draft {
				reply(w, 200, f.view(rel))
				return
			}
		}
		reply(w, 404, map[string]any{"message": "Not Found"})

	case r.Method == "GET" && p == "":
		var all []github.Release
		for i := len(f.releases) - 1; i >= 0; i-- { // newest first
			all = append(all, f.view(f.releases[i]))
		}
		paginate(w, r, all)

	case r.Method == "POST" && p == "":
		var req github.ReleaseRequest
		if err := json.Unmarshal(body, &req); err != nil {
			reply(w, 400, map[string]any{"message": "Problems parsing JSON"})
			return
		}
		for _, rel := range f.releases {
			if rel.TagName == req.TagName && !rel.Draft {
				validationFailed(w, "Release", "tag_name", "already_exists")
				return
			}
		}
		rel := &fakeRelease{Release: github.Release{ID: f.id(), TagName: req.TagName, TargetCommitish: req.TargetCommitish}}
		applyRelease(&rel.Release, req)
		if !rel.Draft {
			rel.Immutable = f.immutable
		}
		f.refreshURLs(rel)
		f.releases = append(f.releases, rel)
		reply(w, 201, f.view(rel))

	case r.Method == "PATCH" && len(parts) == 1:
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		rel := f.release(id)
		if rel == nil {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		var req github.ReleaseRequest
		if err := json.Unmarshal(body, &req); err != nil {
			reply(w, 400, map[string]any{"message": "Problems parsing JSON"})
			return
		}
		if rel.Immutable && req.Draft != nil && *req.Draft {
			validationFailed(w, "Release", "draft", "invalid")
			return
		}
		wasDraft := rel.Draft
		applyRelease(&rel.Release, req)
		if wasDraft && !rel.Draft {
			rel.Immutable = f.immutable
		}
		f.refreshURLs(rel)
		reply(w, 200, f.view(rel))

	case r.Method == "DELETE" && len(parts) == 1 && parts[0] != "":
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		for i, rel := range f.releases {
			if rel.ID == id {
				f.releases = append(f.releases[:i], f.releases[i+1:]...)
				w.WriteHeader(204)
				return
			}
		}
		reply(w, 404, map[string]any{"message": "Not Found"})

	case r.Method == "PATCH" && len(parts) == 2 && parts[0] == "assets":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		rel, i := f.asset(id)
		if rel == nil {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		if rel.Immutable {
			reply(w, 422, map[string]any{"message": "Cannot update asset of an immutable release."})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Name != "" {
			for _, a := range rel.assets {
				if a.Name == req.Name && a.ID != id {
					validationFailed(w, "ReleaseAsset", "name", "already_exists")
					return
				}
			}
			rel.assets[i].Name = req.Name
		}
		f.refreshURLs(rel)
		reply(w, 200, rel.assets[i].Asset)

	case r.Method == "GET" && len(parts) == 2 && parts[1] == "assets":
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		rel := f.release(id)
		if rel == nil {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		paginate(w, r, f.assetViews(rel))

	case r.Method == "POST" && len(parts) == 2 && parts[1] == "assets":
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		rel := f.release(id)
		if rel == nil {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		if r.ContentLength < 0 {
			reply(w, 411, map[string]any{"message": "Length Required"})
			return
		}
		if rel.Immutable {
			reply(w, 422, map[string]any{"message": "Cannot upload assets to an immutable release."})
			return
		}
		name := r.URL.Query().Get("name")
		for _, a := range rel.assets {
			if a.Name == name {
				validationFailed(w, "ReleaseAsset", "name", "already_exists")
				return
			}
		}
		a := f.addAssetLocked(rel, name, body, github.AssetStateUploaded)
		a.ContentType = r.Header.Get("Content-Type")
		reply(w, 201, a.Asset)

	case r.Method == "GET" && len(parts) == 2 && parts[0] == "assets":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if rel, _ := f.asset(id); rel == nil {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		if r.Header.Get("Accept") != "application/octet-stream" {
			f.t.Errorf("asset download without octet-stream Accept")
		}
		http.Redirect(w, r, f.storage.URL+"/blob/"+parts[1], http.StatusFound)

	case r.Method == "DELETE" && len(parts) == 2 && parts[0] == "assets":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		rel, i := f.asset(id)
		if rel == nil {
			reply(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		if rel.Immutable {
			reply(w, 422, map[string]any{"message": "Cannot delete asset from an immutable release."})
			return
		}
		rel.assets = append(rel.assets[:i], rel.assets[i+1:]...)
		w.WriteHeader(204)

	default:
		f.t.Errorf("fake GitHub: unhandled %s %s", r.Method, r.URL.Path)
		reply(w, 404, map[string]any{"message": "Not Found"})
	}
}

func applyRelease(rel *github.Release, req github.ReleaseRequest) {
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
}

func paginate[T any](w http.ResponseWriter, r *http.Request, all []T) {
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	start := (page - 1) * pageSize
	end := min(start+pageSize, len(all))
	items := []T{}
	if start < len(all) {
		items = all[start:end]
	}
	if end < len(all) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	reply(w, 200, items)
}
