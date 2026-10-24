package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

const tag = "v1.0.0"

const (
	linuxName    = "tool_1.0.0_linux_amd64.tar.gz"
	darwinName   = "tool_1.0.0_darwin_arm64.tar.gz"
	checksumName = "tool_1.0.0_checksums.txt"
)

var defaultFiles = map[string]string{
	linuxName:    "linux archive bytes",
	darwinName:   "darwin archive bytes",
	checksumName: "checksums\n",
}

type fixture struct {
	t   *testing.T
	cfg *config.Config
	man *manifest.Manifest
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest.New("tool", "1.0.0", tag, "0123abc")
	for name, content := range defaultFiles {
		if err := os.WriteFile(filepath.Join(root, "dist", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		a := manifest.Artifact{Name: name, Path: "dist/" + name, SHA256: sum(content), Size: int64(len(content))}
		switch name {
		case checksumName:
			a.Kind, a.ID, a.Source = manifest.KindChecksum, "default", manifest.Source{Stage: "checksum"}
		default:
			a.Kind, a.ID, a.Format, a.Source = manifest.KindArchive, "default", "tar.gz", manifest.Source{Stage: "archive"}
			a.OS, a.Arch = "linux", "amd64"
			if name == darwinName {
				a.OS, a.Arch = "darwin", "arm64"
			}
		}
		m.Put(a)
	}
	// A binary, which the default include list does not upload.
	m.Put(manifest.Artifact{Kind: manifest.KindBinary, ID: "default", Name: "tool", Path: "dist/tool", OS: "linux", Arch: "amd64", Source: manifest.Source{Stage: "build"}})
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Root:    root,
		Project: config.Project{Name: "tool"},
		Release: config.Release{GitHub: &config.GitHubRelease{Owner: fakeOwner, Repo: fakeRepo}},
	}
	cfg.ApplyDefaults()
	return &fixture{t: t, cfg: cfg, man: m}
}

func (fx *fixture) gh() *config.GitHubRelease { return fx.cfg.Release.GitHub }

func (fx *fixture) input(c *github.Client) Input {
	return Input{
		Config: fx.cfg, Manifest: fx.man, Client: c, Tag: tag,
		Vars: tmpl.Vars{Project: "tool", Version: "1.0.0", Tag: tag},
		Log:  testLog{fx.t},
	}
}

func (fx *fixture) downloadURL(name string) string {
	for _, a := range fx.man.Artifacts {
		if a.Name == name && a.Kind != manifest.KindBinary {
			return a.DownloadURL
		}
	}
	return ""
}

func statuses(res *Result) map[string]string {
	out := map[string]string{}
	for _, a := range res.Assets {
		out[a.Name] = a.Status
	}
	return out
}

func wantStatuses(t *testing.T, res *Result, want map[string]string) {
	t.Helper()
	got := statuses(res)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("asset %s: status %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("assets %v, want %v", got, want)
	}
}

func all(status string) map[string]string {
	return map[string]string{linuxName: status, darwinName: status, checksumName: status}
}

func TestCreateUploadPublish(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().Notes = "Release {{ .Tag }}"
	fx.gh().Name = "Tool {{ .Version }}"
	fx.gh().MakeLatest = "true"

	var createBody, publishBody map[string]any
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/releases"):
			_ = json.Unmarshal(body, &createBody)
		case r.Method == "PATCH":
			_ = json.Unmarshal(body, &publishBody)
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.Published || res.NoOp {
		t.Fatalf("result %+v", res)
	}
	wantStatuses(t, res, all(StatusUploaded))
	if createBody["draft"] != true || createBody["tag_name"] != tag || createBody["name"] != "Tool 1.0.0" ||
		createBody["body"] != "Release v1.0.0" || createBody["prerelease"] != false {
		t.Fatalf("create body %v", createBody)
	}
	if _, ok := createBody["make_latest"]; ok {
		t.Fatal("make_latest sent while creating a draft")
	}
	if publishBody["draft"] != false || publishBody["make_latest"] != "true" {
		t.Fatalf("publish body %v", publishBody)
	}

	// Order: create, uploads, then publish.
	w := f.writes()
	if len(w) != 5 || !strings.HasSuffix(w[0], "/releases") || !strings.HasPrefix(w[4], "PATCH ") {
		t.Fatalf("writes %v", w)
	}
	for _, l := range w[1:4] {
		if !strings.HasPrefix(l, "POST ") || !strings.HasSuffix(l, "/assets") {
			t.Fatalf("writes %v", w)
		}
	}

	rels := f.byTag(tag)
	if len(rels) != 1 || rels[0].Draft || len(rels[0].assets) != 3 {
		t.Fatalf("server state %+v", rels)
	}
	for name := range defaultFiles {
		want := webBase + "/releases/download/" + tag + "/" + name
		if got := fx.downloadURL(name); got != want {
			t.Errorf("%s download URL %q, want %q", name, got, want)
		}
	}
	if res.ReleaseURL != webBase+"/releases/tag/"+tag {
		t.Errorf("release URL %q", res.ReleaseURL)
	}
	for _, a := range fx.man.Artifacts {
		if a.Kind == manifest.KindBinary && a.DownloadURL != "" {
			t.Error("binary got a download URL although it was not uploaded")
		}
	}
}

func TestRerunIsNoOp(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().Notes = "notes"
	c := f.client(t)
	if _, err := Run(context.Background(), fx.input(c)); err != nil {
		t.Fatal(err)
	}
	f.resetLog()
	res, err := Run(context.Background(), fx.input(c))
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoOp || res.Created || !res.Published || len(res.Actions) != 0 {
		t.Fatalf("result %+v", res)
	}
	wantStatuses(t, res, all(StatusUnchanged))
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("re-run wrote: %v", w)
	}
	if fx.downloadURL(linuxName) == "" {
		t.Fatal("download URL missing after no-op run")
	}
}

func TestReusesExistingDraft(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	// Three other releases push the draft onto a later list page.
	f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName]})
	f.addRelease("v0.9.0", false, false, nil)
	f.addRelease("v0.8.0", false, false, nil)
	f.addRelease("v0.7.0", false, false, nil)
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || !res.Published {
		t.Fatalf("result %+v", res)
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusUploaded, checksumName: StatusUploaded})
	for _, l := range f.requests() {
		if l == "POST /repos/acme/tool/releases" {
			t.Fatal("created a new release instead of reusing the draft")
		}
	}
	if rels := f.byTag(tag); len(rels) != 1 || rels[0].Draft {
		t.Fatalf("server state %+v", rels)
	}
}

func TestDraftConfigKeepsDraftAndSkipsURLs(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().Draft = true
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Published || f.count("PATCH ") != 0 {
		t.Fatalf("result %+v, requests %v", res, f.requests())
	}
	if fx.downloadURL(linuxName) != "" {
		t.Fatal("draft (untagged) URL recorded in the manifest")
	}
	// Re-running is still a no-op.
	f.resetLog()
	res, err = Run(context.Background(), fx.input(f.client(t)))
	if err != nil || !res.NoOp || len(f.writes()) != 0 {
		t.Fatalf("rerun: %+v %v %v", res, err, f.writes())
	}
}

func TestExistingAssetSameDifferentReplace(t *testing.T) {
	t.Run("same content is unchanged", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err != nil {
			t.Fatal(err)
		}
		wantStatuses(t, res, all(StatusUnchanged))
		if w := f.writes(); len(w) != 1 || !strings.HasPrefix(w[0], "PATCH ") {
			t.Fatalf("writes %v (only the publish expected)", w)
		}
	})
	t.Run("different content is an error", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, true, false, map[string]string{linuxName: "other bytes"})
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "replace_existing_assets") {
			t.Fatalf("err = %v", err)
		}
		wantStatuses(t, res, map[string]string{linuxName: StatusFailed, darwinName: StatusUploaded, checksumName: StatusUploaded})
		if res.Published || f.count("PATCH ") != 0 || f.count("DELETE ") != 0 {
			t.Fatalf("published or deleted despite the conflict: %v", f.writes())
		}
		if rels := f.byTag(tag); !rels[0].Draft || string(rels[0].assets[0].data) != "other bytes" {
			t.Fatal("existing asset was modified")
		}
	})
	t.Run("replace_existing_assets replaces", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		fx.gh().ReplaceExistingAssets = true
		f.addRelease(tag, true, false, map[string]string{linuxName: "other bytes"})
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err != nil {
			t.Fatal(err)
		}
		wantStatuses(t, res, map[string]string{linuxName: StatusReplaced, darwinName: StatusUploaded, checksumName: StatusUploaded})
		rel := f.byTag(tag)[0]
		for _, a := range rel.assets {
			if a.Name == linuxName && string(a.data) != defaultFiles[linuxName] {
				t.Fatal("asset not replaced")
			}
		}
		if !res.Published {
			t.Fatal("not published")
		}
	})
}

func TestCompareWithoutDigestDownloadsWithoutToken(t *testing.T) {
	f := newFakeGitHub(t)
	f.digests = false // assets uploaded before GitHub computed digests
	fx := newFixture(t)
	f.addRelease(tag, false, false, map[string]string{
		linuxName:    defaultFiles[linuxName],
		darwinName:   defaultFiles[darwinName],
		checksumName: defaultFiles[checksumName],
	})
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, res, all(StatusUnchanged))
	if !res.NoOp {
		t.Fatalf("not a no-op: %v", f.writes())
	}
	f.mu.Lock()
	auth := append([]string(nil), f.storageAuth...)
	f.mu.Unlock()
	if len(auth) != 3 {
		t.Fatalf("expected 3 downloads, got %d", len(auth))
	}
	for _, a := range auth {
		if a != "" {
			t.Fatalf("Authorization sent to the storage host: %q", a)
		}
	}

	// Same size, different bytes: detected by hashing.
	f2 := newFakeGitHub(t)
	f2.digests = false
	fx2 := newFixture(t)
	tampered := strings.Repeat("X", len(defaultFiles[linuxName]))
	f2.addRelease(tag, false, false, map[string]string{linuxName: tampered})
	_, err = Run(context.Background(), fx2.input(f2.client(t)))
	if err == nil || !strings.Contains(err.Error(), "remote sha256:"+sum(tampered)) {
		t.Fatalf("err = %v", err)
	}
}

func TestRecoverFromUpload502LeavingStarter(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	failed := false
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/assets") && r.URL.Query().Get("name") == darwinName && !failed {
			failed = true
			f.mu.Lock()
			rel := f.byTagLocked(tag)
			f.addAssetLocked(rel, darwinName, nil, github.AssetStateStarter)
			f.mu.Unlock()
			reply(w, 502, map[string]any{"message": "Bad Gateway"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, res, all(StatusUploaded))
	if f.count("DELETE ") != 1 || f.count("POST /repos/acme/tool/releases/") != 4 {
		t.Fatalf("requests %v", f.writes())
	}
	rel := f.byTag(tag)[0]
	if len(rel.assets) != 3 || rel.Draft {
		t.Fatalf("server state %+v", rel)
	}
	for _, a := range rel.assets {
		if a.State != github.AssetStateUploaded {
			t.Fatalf("leftover %s in state %s", a.Name, a.State)
		}
	}
}

func TestUploadLostResponseIsVerifiedNotRepeated(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	failed := false
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Query().Get("name") == linuxName && !failed {
			failed = true
			f.mu.Lock()
			f.addAssetLocked(f.byTagLocked(tag), linuxName, body, github.AssetStateUploaded)
			f.mu.Unlock()
			reply(w, 504, map[string]any{"message": "Gateway Timeout"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, res, all(StatusUploaded))
	if n := f.count("POST /repos/acme/tool/releases/"); n != 3 {
		t.Fatalf("%d uploads, want 3: %v", n, f.writes())
	}
}

func TestUpload422AlreadyExists(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	// The asset list is eventually consistent: the asset is not listed at
	// first, so the upload hits 422 already_exists.
	hidden := true
	f.addRelease(tag, true, false, nil)
	f.mu.Lock()
	f.addAssetLocked(f.byTagLocked(tag), checksumName, []byte(defaultFiles[checksumName]), github.AssetStateUploaded)
	f.mu.Unlock()
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/assets") && hidden {
			hidden = false
			reply(w, 200, []github.Asset{})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, res, all(StatusUploaded))
	if !res.Published {
		t.Fatal("not published")
	}
}

func TestCreateRelease422AlreadyExists(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	// Another run publishes the release between our lookup and our create.
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
			f.addRelease(tag, true, false, nil)
			validationFailed(w, "Release", "tag_name", "already_exists")
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if rels := f.byTag(tag); len(rels) != 1 || rels[0].Draft || len(rels[0].assets) != 3 {
		t.Fatalf("server state %+v", rels)
	}
	wantStatuses(t, res, all(StatusUploaded))
}

func TestCreateFailureDoesNotDuplicateRelease(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
			// The release is created, but the response is lost.
			f.addRelease(tag, true, false, nil)
			reply(w, 502, map[string]any{"message": "Bad Gateway"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST /repos/acme/tool/releases"); n-f.count("POST /repos/acme/tool/releases/") != 1 {
		t.Fatalf("create POSTed %d times", n)
	}
	if rels := f.byTag(tag); len(rels) != 1 {
		t.Fatalf("%d releases for the tag", len(rels))
	}
	if !res.Created || !res.Published {
		t.Fatalf("result %+v", res)
	}
}

func TestCreateFailureRetriesWhenNothingWasCreated(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	n := 0
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
			n++
			if n == 1 {
				reply(w, 500, map[string]any{"message": "boom"})
				return true
			}
		}
		return false
	}
	if _, err := Run(context.Background(), fx.input(f.client(t))); err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(f.byTag(tag)) != 1 {
		t.Fatalf("creates=%d releases=%d", n, len(f.byTag(tag)))
	}
}

func TestRateLimitRetries(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	var got429, got403 bool
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/releases/tags/") && !got429:
			got429 = true
			w.Header().Set("Retry-After", "5")
			reply(w, 429, map[string]any{"message": "Too Many Requests"})
			return true
		case r.Method == "PATCH" && !got403:
			got403 = true
			w.Header().Set("Retry-After", "2")
			reply(w, 403, map[string]any{"message": "You have exceeded a secondary rate limit."})
			return true
		}
		return false
	}
	start := time.Now()
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("slept for real")
	}
	if !res.Published || f.count("PATCH ") != 2 {
		t.Fatalf("result %+v, %v", res, f.writes())
	}
	f.mu.Lock()
	sleeps := append([]time.Duration(nil), f.sleeps...)
	f.mu.Unlock()
	if len(sleeps) != 2 || sleeps[0] != 5*time.Second || sleeps[1] != 2*time.Second {
		t.Fatalf("sleeps %v", sleeps)
	}
}

func TestImmutablePublishedRelease(t *testing.T) {
	complete := map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]}
	t.Run("complete is a no-op", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, false, true, complete)
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err != nil {
			t.Fatal(err)
		}
		if !res.NoOp || len(f.writes()) != 0 {
			t.Fatalf("result %+v writes %v", res, f.writes())
		}
		wantStatuses(t, res, all(StatusUnchanged))
	})
	t.Run("missing asset is an error", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, false, true, map[string]string{linuxName: defaultFiles[linuxName]})
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("err = %v", err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("wrote to an immutable release: %v", w)
		}
		wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusFailed, checksumName: StatusFailed})
	})
	t.Run("different asset is an error even with replace", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		fx.gh().ReplaceExistingAssets = true
		changed := map[string]string{linuxName: "x", darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]}
		f.addRelease(tag, false, true, changed)
		_, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "immutable") || len(f.writes()) != 0 {
			t.Fatalf("err = %v, writes %v", err, f.writes())
		}
	})
	t.Run("publishing makes it immutable; re-run is a no-op", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.immutable = true
		fx := newFixture(t)
		c := f.client(t)
		if _, err := Run(context.Background(), fx.input(c)); err != nil {
			t.Fatal(err)
		}
		if rel := f.byTag(tag)[0]; !rel.Immutable || len(rel.assets) != 3 {
			t.Fatalf("server state %+v", rel)
		}
		f.resetLog()
		res, err := Run(context.Background(), fx.input(c))
		if err != nil || !res.NoOp || len(f.writes()) != 0 {
			t.Fatalf("rerun: %+v %v %v", res, err, f.writes())
		}
	})
}

func TestPublishedMutableReleaseGetsMissingAsset(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.addRelease(tag, false, false, map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName]})
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusUnchanged, checksumName: StatusUploaded})
	if f.count("PATCH ") != 0 {
		t.Fatalf("published release was patched: %v", f.writes())
	}
	if fx.downloadURL(checksumName) != webBase+"/releases/download/"+tag+"/"+checksumName {
		t.Fatal("download URL not recorded")
	}
}

func TestPartialFailureDoesNotPublish(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Query().Get("name") == darwinName {
			reply(w, 403, map[string]any{"message": "Resource not accessible"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "1 of 3 assets failed") || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Published {
		t.Fatalf("result %+v", res)
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusUploaded, darwinName: StatusFailed, checksumName: StatusUploaded})
	for _, a := range res.Assets {
		if (a.Status == StatusFailed) != (a.Err != nil) {
			t.Fatalf("asset %s: status %s err %v", a.Name, a.Status, a.Err)
		}
	}
	if f.count("PATCH ") != 0 || !f.byTag(tag)[0].Draft {
		t.Fatal("release was published despite a failed asset")
	}
	if n := f.count("POST /repos/acme/tool/releases/"); n != 3 {
		t.Fatalf("a non-retryable 403 was retried: %d uploads", n)
	}
	if fx.downloadURL(linuxName) != "" {
		t.Fatal("download URL recorded for an unpublished release")
	}

	// The next run resumes: uploaded assets are unchanged.
	f.hook = nil
	f.resetLog()
	res, err = Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusUploaded, checksumName: StatusUnchanged})
	if !res.Published {
		t.Fatal("not published after resume")
	}
}

func TestUploadGivesUpAfterLimitedAttempts(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Query().Get("name") == linuxName {
			reply(w, 503, map[string]any{"message": "unavailable"})
			return true
		}
		return false
	}
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("err = %v", err)
	}
	n := 0
	for _, l := range f.requests() {
		if l == "POST /repos/acme/tool/releases/"+fmt.Sprint(f.byTag(tag)[0].ID)+"/assets" {
			n++
		}
	}
	if n != 5 { // 3 attempts for the failing asset + 2 other assets
		t.Fatalf("%d uploads", n)
	}
}

func TestTokenNotInErrors(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		reply(w, 401, map[string]any{"message": "Bad credentials " + r.Header.Get("Authorization")})
		return true
	}
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(fmt.Sprintf("%v %+v", err, err), fakeToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	t.Run("without client", func(t *testing.T) {
		fx := newFixture(t)
		in := fx.input(nil)
		in.DryRun = true
		res, err := Run(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !res.DryRun || !res.Created || res.NoOp || len(res.Actions) != 5 {
			t.Fatalf("result %+v", res)
		}
		wantStatuses(t, res, all(StatusPlanned))
		if fx.downloadURL(linuxName) != "" {
			t.Fatal("dry run modified the manifest")
		}
	})
	t.Run("with client and new release", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		in := fx.input(f.client(t))
		in.DryRun = true
		res, err := Run(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("dry run wrote: %v", w)
		}
		wantStatuses(t, res, all(StatusPlanned))
		if len(f.byTag(tag)) != 0 {
			t.Fatal("release created")
		}
	})
	t.Run("with client and existing draft", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName], darwinName: "different"})
		f.mu.Lock()
		st := f.addAssetLocked(f.byTagLocked(tag), checksumName, nil, github.AssetStateStarter)
		st.CreatedAt, st.UpdatedAt = time.Now().Add(-time.Hour), time.Now().Add(-time.Hour) // a leftover
		f.mu.Unlock()
		fx.gh().ReplaceExistingAssets = true
		in := fx.input(f.client(t))
		in.DryRun = true
		res, err := Run(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("dry run wrote: %v", w)
		}
		wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusPlanned, checksumName: StatusPlanned})
		if res.Created || res.NoOp {
			t.Fatalf("result %+v", res)
		}
		joined := strings.Join(res.Actions, "\n")
		for _, want := range []string{"replace " + darwinName, "delete incomplete " + checksumName, "publish release"} {
			if !strings.Contains(joined, want) {
				t.Errorf("actions missing %q:\n%s", want, joined)
			}
		}
	})
	t.Run("conflict is reported", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, true, false, map[string]string{linuxName: "different"})
		in := fx.input(f.client(t))
		in.DryRun = true
		if _, err := Run(context.Background(), in); err == nil {
			t.Fatal("dry run hid a conflict")
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("dry run wrote: %v", w)
		}
	})
}

func TestPreflightValidation(t *testing.T) {
	cases := []struct {
		name string
		mod  func(fx *fixture, in *Input)
		want string
	}{
		{"missing tag", func(fx *fixture, in *Input) { in.Tag = "" }, "tag is required"},
		{"bad tag", func(fx *fixture, in *Input) { in.Tag = "v1..0" }, "not a valid git tag"},
		{"tag differs from manifest", func(fx *fixture, in *Input) { in.Tag = "v2.0.0" }, "does not match the manifest"},
		{"missing repository", func(fx *fixture, in *Input) { fx.gh().Owner = "" }, "owner and name are required"},
		{"tampered file", func(fx *fixture, in *Input) {
			_ = os.WriteFile(filepath.Join(fx.cfg.Root, "dist", linuxName), []byte("evil archive bytes!"), 0o644)
		}, "modified after the build"},
		{"tampered same size", func(fx *fixture, in *Input) {
			_ = os.WriteFile(filepath.Join(fx.cfg.Root, "dist", linuxName), []byte(strings.Repeat("z", len(defaultFiles[linuxName]))), 0o644)
		}, "has sha256"},
		{"missing file", func(fx *fixture, in *Input) { _ = os.Remove(filepath.Join(fx.cfg.Root, "dist", darwinName)) }, "no such file (the manifest lists it"},
		{"no sha256 in manifest", func(fx *fixture, in *Input) {
			for i := range fx.man.Artifacts {
				if fx.man.Artifacts[i].Kind == manifest.KindChecksum {
					fx.man.Artifacts[i].SHA256 = ""
				}
			}
		}, "no sha256"},
		{"bad asset name", func(fx *fixture, in *Input) {
			fx.man.Artifacts = append(fx.man.Artifacts, manifest.Artifact{Kind: manifest.KindChecksum, Name: ".hidden", Path: "dist/x", Source: manifest.Source{Stage: "checksum"}})
		}, "asset name must match"},
		{"name collision", func(fx *fixture, in *Input) {
			for _, b := range fx.man.Artifacts {
				if b.Name == checksumName {
					b.Name = strings.ToUpper(b.Name)
					fx.man.Artifacts = append(fx.man.Artifacts, b)
					return
				}
			}
		}, "collides"},
		{"nothing to upload", func(fx *fixture, in *Input) {
			fx.gh().Include = []string{"binary"}
			fx.man.RemoveKind(manifest.KindBinary)
		}, "no artifacts of the included kinds"},
		{"no client", func(fx *fixture, in *Input) { in.Client = nil }, "client"},
		{"missing notes file", func(fx *fixture, in *Input) { fx.gh().NotesFile = "NOTES.md" }, "notes_file"},
		{"disabled", func(fx *fixture, in *Input) { fx.cfg.Release.GitHub = nil }, "not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			fx := newFixture(t)
			in := fx.input(f.client(t))
			tc.mod(fx, &in)
			_, err := Run(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if r := f.requests(); len(r) != 0 {
				t.Fatalf("network used before validation passed: %v", r)
			}
		})
	}
}

func TestFileChangedAfterVerificationIsNotUploadedSilently(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	// Swap the file's bytes (same size) once the release has been created,
	// i.e. after the preflight hash.
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
			_ = os.WriteFile(filepath.Join(fx.cfg.Root, "dist", linuxName), []byte(strings.Repeat("z", len(defaultFiles[linuxName]))), 0o644)
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "changed while it was uploaded") {
		t.Fatalf("err = %v", err)
	}
	if res.Published {
		t.Fatal("published")
	}
	for _, a := range f.byTag(tag)[0].assets {
		if a.Name == linuxName {
			t.Fatal("bad upload was left on the release")
		}
	}
}

func TestNotesFileNamePrereleaseAndTargetCommitish(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	const rc = "v1.0.0-rc.1"
	fx.man.Tag = rc
	if err := os.WriteFile(filepath.Join(fx.cfg.Root, "NOTES.md"), []byte("## Changes {{ not a template }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.gh().NotesFile = "NOTES.md"
	fx.gh().TargetCommitish = "release-branch"
	fx.gh().MakeLatest = "true"
	var createBody, patchBody map[string]any
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases":
			_ = json.Unmarshal(body, &createBody)
		case r.Method == "PATCH":
			_ = json.Unmarshal(body, &patchBody)
		}
		return false
	}
	in := fx.input(f.client(t))
	in.Tag = rc
	res, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if createBody["prerelease"] != true || createBody["body"] != "## Changes {{ not a template }}\n" ||
		createBody["target_commitish"] != "release-branch" || createBody["name"] != rc {
		t.Fatalf("create body %v", createBody)
	}
	if _, ok := patchBody["make_latest"]; ok {
		t.Fatalf("make_latest sent for a prerelease: %v", patchBody)
	}
	if !res.Published || !f.byTag(rc)[0].Prerelease {
		t.Fatal("prerelease not published")
	}
}

// TestPublishedReleaseIsNeverEdited: changed notes, title or prerelease
// flag in the configuration never edit a published release, mutable or
// immutable; the difference is reported and the run succeeds, so later
// stages (formula, tap) can resume.
func TestPublishedReleaseIsNeverEdited(t *testing.T) {
	complete := map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]}
	for _, immutable := range []bool{false, true} {
		t.Run(fmt.Sprintf("immutable=%t", immutable), func(t *testing.T) {
			f := newFakeGitHub(t)
			fx := newFixture(t)
			rel := f.addRelease(tag, false, immutable, complete)
			rel.Body, rel.Name = "old notes", "old title"
			fx.gh().Notes = "new notes"
			fx.gh().Prerelease = "true"
			lb := &logBuf{t: t}
			in := fx.input(f.client(t))
			in.Log = lb
			res, err := Run(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("wrote to a published release: %v", w)
			}
			wantStatuses(t, res, all(StatusUnchanged))
			got := f.byTag(tag)[0]
			if !res.NoOp || !res.Published || got.Body != "old notes" || got.Name != "old title" || got.Prerelease || got.Draft {
				t.Fatalf("result %+v, release %+v", res, got.Release)
			}
			for _, want := range []string{"name, notes, prerelease", "never edited"} {
				if !strings.Contains(lb.String(), want) {
					t.Errorf("log does not report %q:\n%s", want, lb.String())
				}
			}
		})
	}
}

// TestDigestConflictOnPublishedRelease: an existing asset whose digest
// differs from the local build stops the run on a published mutable
// release too, without touching the asset or the release.
func TestDigestConflictOnPublishedRelease(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	// Same size, other content: only the digest tells them apart.
	other := strings.Repeat("x", len(defaultFiles[linuxName]))
	f.addRelease(tag, false, false, map[string]string{linuxName: other, darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "sha256:") || !strings.Contains(err.Error(), "left as it was") {
		t.Fatalf("err = %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("wrote despite the conflict: %v", w)
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusFailed, darwinName: StatusUnchanged, checksumName: StatusUnchanged})
	for _, a := range f.byTag(tag)[0].assets {
		if a.Name == linuxName && string(a.data) != other {
			t.Fatal("the published asset was modified")
		}
	}
}

// TestDraftNotesAreUpdatedBeforePublishing: a draft is still being
// prepared, so its notes and title follow the configuration and are
// published with it in one request.
func TestDraftNotesAreUpdatedBeforePublishing(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	rel := f.addRelease(tag, true, false, nil)
	rel.Body = "old notes"
	fx.gh().Notes = "new notes"
	var patches []map[string]any
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == http.MethodPatch {
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			patches = append(patches, m)
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || patches[0]["body"] != "new notes" || patches[0]["draft"] != false {
		t.Fatalf("patches %v", patches)
	}
	if got := f.byTag(tag)[0]; got.Body != "new notes" || got.Draft || !res.Published {
		t.Fatalf("release %+v", got.Release)
	}
}

func TestIsPrerelease(t *testing.T) {
	for tag, want := range map[string]bool{
		"v1.0.0": false, "1.2.3": false, "v1.0.0-rc.1": true, "v2.0.0-beta": true,
		"v1.0.0+build.5": false, "v1.0.0-alpha+build": true, "v1.0": false, "release-1": false, "v01.0.0-rc1": false,
	} {
		if got := IsPrerelease(tag); got != want {
			t.Errorf("IsPrerelease(%q) = %t, want %t", tag, got, want)
		}
	}
}

func TestContextCancelled(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, fx.input(f.client(t)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateNotesOnlyWithoutExplicitNotes(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		fx.gh().GenerateNotes = true
		if explicit {
			fx.gh().Notes = "hand written"
		}
		var createBody map[string]any
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/releases") {
				_ = json.Unmarshal(body, &createBody)
			}
			return false
		}
		if _, err := Run(context.Background(), fx.input(f.client(t))); err != nil {
			t.Fatal(err)
		}
		_, generated := createBody["generate_release_notes"]
		if generated == explicit {
			t.Errorf("explicit notes %v: generate_release_notes sent = %v", explicit, generated)
		}
	}
}
