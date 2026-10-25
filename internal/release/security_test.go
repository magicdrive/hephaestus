package release

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
)

// logBuf collects the log (and still shows it in the test output).
type logBuf struct {
	t  *testing.T
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// --- F2: notes_file is read through os.Root ---

func TestNotesFileSymlinkOutsideRootIsRejected(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET_FROM_OUTSIDE_ROOT=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(root string) string{
		"file link": func(root string) string {
			if err := os.Symlink(outside, filepath.Join(root, "CHANGELOG.md")); err != nil {
				t.Fatal(err)
			}
			return "CHANGELOG.md"
		},
		"relative file link": func(root string) string {
			rel, err := filepath.Rel(root, outside)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, filepath.Join(root, "CHANGELOG.md")); err != nil {
				t.Fatal(err)
			}
			return "CHANGELOG.md"
		},
		"parent directory link": func(root string) string {
			if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "docs")); err != nil {
				t.Fatal(err)
			}
			return "docs/secret.txt"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			fx := newFixture(t)
			fx.gh().NotesFile = setup(fx.cfg.Root)
			_, err := Run(context.Background(), fx.input(f.client(t)))
			if err == nil || !strings.Contains(err.Error(), "notes_file") || !strings.Contains(err.Error(), "escapes") {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "SECRET_FROM_OUTSIDE_ROOT") {
				t.Fatal("outside content leaked into the error")
			}
			if r := f.requests(); len(r) != 0 {
				t.Fatalf("network used: %v", r)
			}
		})
	}
}

func TestNotesFileInsideRoot(t *testing.T) {
	t.Run("link inside the project is followed", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		if err := os.MkdirAll(filepath.Join(fx.cfg.Root, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fx.cfg.Root, "docs", "CHANGES.md"), []byte("inside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("docs", "CHANGES.md"), filepath.Join(fx.cfg.Root, "NOTES.md")); err != nil {
			t.Fatal(err)
		}
		fx.gh().NotesFile = "NOTES.md"
		if _, err := Run(context.Background(), fx.input(f.client(t))); err != nil {
			t.Fatal(err)
		}
		if got := f.byTag(tag)[0].Body; got != "inside\n" {
			t.Fatalf("body %q", got)
		}
	})
	t.Run("directory is rejected", func(t *testing.T) {
		fx := newFixture(t)
		fx.gh().NotesFile = "dist"
		_, err := Run(context.Background(), fx.input(newFakeGitHub(t).client(t)))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("size limit is kept", func(t *testing.T) {
		fx := newFixture(t)
		if err := os.WriteFile(filepath.Join(fx.cfg.Root, "BIG.md"), []byte(strings.Repeat("x", maxNotesFile+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		fx.gh().NotesFile = "BIG.md"
		_, err := Run(context.Background(), fx.input(newFakeGitHub(t).client(t)))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err = %v", err)
		}
	})
}

// --- F12: concurrent runs, unexpected assets, replacing published assets ---

// noReleaseDeleted fails when any release (not asset) was deleted.
func noReleaseDeleted(t *testing.T, f *fakeGitHub) {
	t.Helper()
	for _, l := range f.writes() {
		if p, ok := strings.CutPrefix(l, "DELETE /repos/acme/tool/releases/"); ok && !strings.HasPrefix(p, "assets/") {
			t.Fatalf("a release was deleted: %s", l)
		}
	}
}

func TestClientCannotDeleteReleases(t *testing.T) {
	if _, ok := reflect.TypeOf(&github.Client{}).MethodByName("DeleteRelease"); ok {
		t.Fatal("github.Client has DeleteRelease: hephaestus must never delete a release")
	}
}

func TestConcurrentDraftLoserStopsWithoutDeleting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published bool
	}{
		{"lower id draft", false},
		{"published release", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			fx := newFixture(t)
			var other *fakeRelease
			f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" && other == nil {
					// A concurrent run created its draft first (lower id);
					// it may already have published it.
					other = f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName]})
					f.mu.Lock()
					f.route(w, r, body)
					other.Draft = !tc.published
					f.mu.Unlock()
					return true
				}
				return false
			}
			res, err := Run(context.Background(), fx.input(f.client(t)))
			if err == nil || !strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), "nothing was deleted") {
				t.Fatalf("err = %v", err)
			}
			if res != nil && res.Published {
				t.Fatal("published")
			}
			noReleaseDeleted(t, f)
			if n := f.count("POST /repos/acme/tool/releases/"); n != 0 {
				t.Fatalf("uploaded %d assets after the conflict", n)
			}
			if n := f.count("PATCH "); n != 0 {
				t.Fatalf("edited a release after the conflict: %v", f.writes())
			}
			rels := f.byTag(tag)
			if len(rels) != 2 || rels[0].ID != other.ID || rels[0].Draft == tc.published || len(rels[0].assets) != 1 || !rels[1].Draft || len(rels[1].assets) != 0 {
				t.Fatalf("server state changed: %d releases", len(rels))
			}

			// The re-run continues with the release that won; the empty
			// draft is left for the owner.
			f.hook = nil
			f.resetLog()
			res, err = Run(context.Background(), fx.input(f.client(t)))
			if err != nil {
				t.Fatal(err)
			}
			noReleaseDeleted(t, f)
			if res.ReleaseID != other.ID || !res.Published {
				t.Fatalf("result %+v", res)
			}
			if rels := f.byTag(tag); len(rels) != 2 || !rels[1].Draft {
				t.Fatal("the leftover draft was touched")
			}
		})
	}
}

func TestConcurrentDraftWinnerKeepsItsDraft(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().Draft = true
	created := false
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" && !created {
			created = true
			f.mu.Lock()
			f.route(w, r, body)
			f.mu.Unlock()
			// The concurrent run's draft comes later (higher id).
			f.addRelease(tag, true, false, nil)
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || f.count("DELETE ") != 0 {
		t.Fatalf("result %+v writes %v", res, f.writes())
	}
	if rels := f.byTag(tag); len(rels) != 2 || !rels[1].Draft {
		t.Fatal("the concurrent run's draft was touched")
	}
	if rels := f.byTag(tag); len(rels) != 2 || rels[0].ID != res.ReleaseID {
		t.Fatal("the winner's draft was not kept")
	}
}

func TestUnexpectedAssetsOnDraft(t *testing.T) {
	t.Run("draft to be published is an error", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, true, false, map[string]string{"install.sh": "curl evil | sh", linuxName: defaultFiles[linuxName]})
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "install.sh") || !strings.Contains(err.Error(), "not in the manifest") {
			t.Fatalf("err = %v", err)
		}
		if res == nil || res.Published {
			t.Fatalf("result %+v", res)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("writes %v (nothing may be written, nor deleted)", w)
		}
		if rel := f.byTag(tag)[0]; !rel.Draft || len(rel.assets) != 2 {
			t.Fatal("draft was changed")
		}
	})
	t.Run("draft: true only warns", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		fx.gh().Draft = true
		f.addRelease(tag, true, false, map[string]string{"install.sh": "x"})
		lb := &logBuf{t: t}
		in := fx.input(f.client(t))
		in.Log = lb
		if _, err := Run(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lb.String(), "warning: release") || !strings.Contains(lb.String(), "install.sh") {
			t.Fatal("no warning")
		}
	})
	t.Run("published release only warns", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, false, false, map[string]string{"legacy.tar.gz": "x"})
		if _, err := Run(context.Background(), fx.input(f.client(t))); err != nil {
			t.Fatal(err)
		}
	})
}

// TestPublishedReleaseAssetsAreNeverReplacedOrDeleted: on a published
// mutable release, a differing asset is an error even with
// replace_existing_assets, and an incomplete asset is reported, not
// deleted. Nothing is written.
func TestPublishedReleaseAssetsAreNeverReplacedOrDeleted(t *testing.T) {
	t.Run("differing asset with replace_existing_assets", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		fx.gh().ReplaceExistingAssets = true
		f.addRelease(tag, false, false, map[string]string{linuxName: "other bytes", darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
		res, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "never replaced") || !strings.Contains(err.Error(), "left as it was") {
			t.Fatalf("err = %v", err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("wrote to a published release: %v", w)
		}
		wantStatuses(t, res, map[string]string{linuxName: StatusFailed, darwinName: StatusUnchanged, checksumName: StatusUnchanged})
		for _, a := range f.byTag(tag)[0].assets {
			if a.Name == linuxName && string(a.data) != "other bytes" {
				t.Fatal("published asset modified")
			}
		}
	})
	t.Run("old incomplete asset", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		rel := f.addRelease(tag, false, false, map[string]string{linuxName: defaultFiles[linuxName], checksumName: defaultFiles[checksumName]})
		f.mu.Lock()
		st := f.addAssetLocked(rel, darwinName, nil, github.AssetStateStarter)
		st.CreatedAt, st.UpdatedAt = time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)
		f.mu.Unlock()
		_, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "never deleted") {
			t.Fatalf("err = %v", err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("wrote to a published release: %v", w)
		}
	})
	t.Run("incomplete asset appears after a failed upload", func(t *testing.T) {
		f := newFakeGitHub(t)
		fx := newFixture(t)
		f.addRelease(tag, false, false, map[string]string{linuxName: defaultFiles[linuxName], checksumName: defaultFiles[checksumName]})
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == "POST" && r.URL.Query().Get("name") == darwinName {
				f.mu.Lock()
				st := f.addAssetLocked(f.byTagLocked(tag), darwinName, nil, github.AssetStateStarter)
				st.CreatedAt, st.UpdatedAt = time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)
				f.mu.Unlock()
				reply(w, 502, map[string]any{"message": "Bad Gateway"})
				return true
			}
			return false
		}
		_, err := Run(context.Background(), fx.input(f.client(t)))
		if err == nil || !strings.Contains(err.Error(), "never deleted") {
			t.Fatalf("err = %v", err)
		}
		if n := f.count("DELETE "); n != 0 {
			t.Fatalf("deleted from a published release: %v", f.writes())
		}
	})
}

func TestDraftReplaceStillDeletesThenUploads(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().ReplaceExistingAssets = true
	f.addRelease(tag, true, false, map[string]string{linuxName: "other bytes"})
	if _, err := Run(context.Background(), fx.input(f.client(t))); err != nil {
		t.Fatal(err)
	}
	for _, l := range f.writes() {
		if strings.HasPrefix(l, "PATCH ") && strings.Contains(l, "/assets/") {
			t.Fatalf("draft asset renamed: %v", f.writes())
		}
	}
}

// --- F13: target_commitish defaults to the built commit ---

const fullSHA = "0123456789abcdef0123456789abcdef01234567"

func TestTargetCommitishDefaultsToCommit(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	var createBody map[string]any
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
			_ = json.Unmarshal(body, &createBody)
		}
		return false
	}
	lb := &logBuf{t: t}
	in := fx.input(f.client(t))
	in.Vars.Commit = strings.ToUpper(fullSHA)
	in.Log = lb
	if _, err := Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if createBody["target_commitish"] != fullSHA {
		t.Fatalf("create body %v", createBody)
	}
	if strings.Contains(lb.String(), "default branch") {
		t.Fatal("warned although the commit is known")
	}
}

func TestTargetCommitishConfiguredWins(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().TargetCommitish = "release-branch"
	var createBody map[string]any
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
			_ = json.Unmarshal(body, &createBody)
		}
		return false
	}
	in := fx.input(f.client(t))
	in.Vars.Commit = fullSHA
	if _, err := Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if createBody["target_commitish"] != "release-branch" {
		t.Fatalf("create body %v", createBody)
	}
}

func TestTargetCommitishSentWhenPublishingExistingDraft(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.addRelease(tag, true, false, nil)
	var patchBody map[string]any
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "PATCH" {
			_ = json.Unmarshal(body, &patchBody)
		}
		return false
	}
	in := fx.input(f.client(t))
	in.Vars.Commit = fullSHA
	if _, err := Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if patchBody["draft"] != false || patchBody["target_commitish"] != fullSHA {
		t.Fatalf("publish body %v", patchBody)
	}
}

func TestUnknownCommitWarnsOnlyWhenTagIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		commit     string
		tagExists  bool
		wantWarn   bool
		wantTarget bool
	}{
		{"no commit, no tag", "", false, true, false},
		{"no commit, tag exists", "", true, false, false},
		{"short commit is not used", "0123abc", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			if tc.tagExists {
				f.tags = map[string]string{tag: strings.Repeat("a", 40)}
			}
			fx := newFixture(t)
			var createBody map[string]any
			f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" {
					_ = json.Unmarshal(body, &createBody)
				}
				return false
			}
			lb := &logBuf{t: t}
			in := fx.input(f.client(t))
			in.Vars.Commit = tc.commit
			in.Log = lb
			if _, err := Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(lb.String(), "HEAD of the default branch"); got != tc.wantWarn {
				t.Fatalf("warned = %t, want %t", got, tc.wantWarn)
			}
			if _, got := createBody["target_commitish"]; got != tc.wantTarget {
				t.Fatalf("create body %v", createBody)
			}
		})
	}
}

// --- N11: an existing tag must point to the built commit ---

func TestExistingTagMustPointToBuiltCommit(t *testing.T) {
	const other = "ffffffffffffffffffffffffffffffffffffffff"
	const annotated = "1111111111111111111111111111111111111111"
	for _, tc := range []struct {
		name      string
		tags      map[string]string
		objects   map[string]string
		commit    string
		published bool
		wantErr   bool
		wantWarn  string
	}{
		{"lightweight tag matches", map[string]string{tag: fullSHA}, nil, fullSHA, false, false, ""},
		{"lightweight tag differs", map[string]string{tag: other}, nil, fullSHA, false, true, ""},
		{"annotated tag matches", map[string]string{tag: annotated}, map[string]string{annotated: fullSHA}, fullSHA, false, false, ""},
		{"annotated tag differs", map[string]string{tag: annotated}, map[string]string{annotated: other}, fullSHA, false, true, ""},
		{"published release, tag differs", map[string]string{tag: other}, nil, fullSHA, true, true, ""},
		{"commit unknown: warning only", map[string]string{tag: other}, nil, "", false, false, "was not verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.tags, f.tagObjects = tc.tags, tc.objects
			if tc.published {
				f.addRelease(tag, false, false, nil)
			}
			fx := newFixture(t)
			lb := &logBuf{t: t}
			in := fx.input(f.client(t))
			in.Vars.Commit = tc.commit
			in.Log = lb
			_, err := Run(context.Background(), in)
			if tc.wantErr {
				want := "tag " + tag + " points to " + other + ", but the artifacts were built from " + fullSHA
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v", err)
				}
				if w := f.writes(); len(w) != 0 {
					t.Fatalf("wrote before failing: %v", w)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantWarn != "" && !strings.Contains(lb.String(), tc.wantWarn) {
				t.Fatal("no warning")
			}
		})
	}
}

// --- N12: concurrent drafts' notes and uploads in progress ---

// TestLostCreateDoesNotClaimAnotherRunsDraft: the create's response is
// lost and the re-read finds a draft another run created. It is used (as
// the lowest id draft for the tag) but never deleted, and its assets are
// only compared, not replaced.
func TestLostCreateDoesNotClaimAnotherRunsDraft(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	var other *fakeRelease
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" && other == nil {
			other = f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName]})
			reply(w, http.StatusBadGateway, map[string]any{"message": "Bad Gateway"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	noReleaseDeleted(t, f)
	if res.ReleaseID != other.ID {
		t.Fatalf("release %d, want %d", res.ReleaseID, other.ID)
	}
	if n := f.count("POST /repos/acme/tool/releases"); n != 1+2 {
		t.Fatalf("writes %v", f.writes())
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusUploaded, checksumName: StatusUploaded})
}

// TestCheckBeforePublish: the draft is re-read right before it is
// published; what changed meanwhile decides whether it is published.
func TestCheckBeforePublish(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *fakeGitHub, rel *fakeRelease)
		wantErr string // "" = no error; the release must not be PATCHed
	}{
		{"published meanwhile is not edited", func(f *fakeGitHub, rel *fakeRelease) {
			rel.Draft, rel.Immutable = false, true
			f.refreshURLs(rel)
		}, ""},
		{"asset added meanwhile", func(f *fakeGitHub, rel *fakeRelease) {
			f.addAssetLocked(rel, "extra.txt", []byte("x"), github.AssetStateUploaded)
		}, "not in the manifest"},
		{"asset changed meanwhile", func(f *fakeGitHub, rel *fakeRelease) {
			for _, a := range rel.assets {
				if a.Name == darwinName {
					d := "sha256:" + sum("other")
					a.Digest = &d
				}
			}
		}, "missing or changed"},
		{"asset removed meanwhile", func(f *fakeGitHub, rel *fakeRelease) {
			rel.assets = rel.assets[1:]
		}, "missing or changed"},
		{"another release published meanwhile", func(f *fakeGitHub, rel *fakeRelease) {
			o := &fakeRelease{Release: github.Release{ID: f.id(), TagName: tag}}
			f.refreshURLs(o)
			f.releases = append(f.releases, o)
		}, "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			fx := newFixture(t)
			changed := false
			f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if r.Method == "GET" && regexp.MustCompile(`^/repos/acme/tool/releases/\d+$`).MatchString(r.URL.Path) && !changed {
					changed = true
					f.mu.Lock()
					tc.change(f, f.releases[0])
					f.mu.Unlock()
				}
				return false
			}
			res, err := Run(context.Background(), fx.input(f.client(t)))
			if !changed {
				t.Fatal("the release was not re-read before publishing")
			}
			if n := f.count("PATCH "); n != 0 {
				t.Fatalf("release edited: %v", f.writes())
			}
			noReleaseDeleted(t, f)
			if tc.wantErr == "" {
				if err != nil || !res.Published {
					t.Fatalf("err %v result %+v", err, res)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v", err)
			}
			if !f.releases[0].Draft {
				t.Fatal("published")
			}
		})
	}
}

func TestRecentStarterAssetIsNotDeleted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		wantErr bool
	}{
		{"recent: upload may be in progress", 2 * time.Minute, true},
		{"old: leftover is deleted", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			fx := newFixture(t)
			rel := f.addRelease(tag, true, false, nil)
			f.mu.Lock()
			st := f.addAssetLocked(rel, darwinName, nil, github.AssetStateStarter)
			st.CreatedAt = time.Now().Add(-tc.age - time.Minute)
			st.UpdatedAt = time.Now().Add(-tc.age)
			f.mu.Unlock()
			res, err := Run(context.Background(), fx.input(f.client(t)))
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				wantStatuses(t, res, all(StatusUploaded))
				return
			}
			if err == nil || !strings.Contains(err.Error(), "another upload may be in progress") {
				t.Fatalf("err = %v", err)
			}
			if f.count("DELETE ") != 0 || res.Published {
				t.Fatalf("deleted or published: %v", f.writes())
			}
			if statuses(res)[darwinName] != StatusFailed {
				t.Fatalf("statuses %v", statuses(res))
			}
		})
	}
}

// publishOnFirstGet makes a concurrent run publish release rel when this
// run first re-reads it by id; change, when set, runs at the same moment.
func publishOnFirstGet(f *fakeGitHub, change func(rel *fakeRelease)) *bool {
	done := false
	re := regexp.MustCompile(`^/repos/acme/tool/releases/\d+$`)
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "GET" && re.MatchString(r.URL.Path) && !done {
			done = true
			f.mu.Lock()
			rel := f.releases[0]
			rel.Draft = false
			if change != nil {
				change(rel)
			}
			f.refreshURLs(rel)
			f.mu.Unlock()
		}
		return false
	}
	return &done
}

// TestDraftPublishedBeforeAssetDeleteKeepsAsset: a draft asset about to be
// replaced is not deleted when a concurrent run published the release
// since it was read.
func TestDraftPublishedBeforeAssetDeleteKeepsAsset(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().ReplaceExistingAssets = true
	f.addRelease(tag, true, false, map[string]string{linuxName: "other bytes", darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
	done := publishOnFirstGet(f, nil)
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if !*done || err == nil || !strings.Contains(err.Error(), "published meanwhile") {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("DELETE "); n != 0 {
		t.Fatalf("deleted from a published release: %v", f.writes())
	}
}

// TestDraftConfigDoesNotEditReleasePublishedMeanwhile: with draft: true,
// the title and notes of a draft are updated only after re-reading it; a
// release a concurrent run published meanwhile is not edited.
func TestDraftConfigDoesNotEditReleasePublishedMeanwhile(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().Draft = true
	fx.gh().Notes = "new notes"
	f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
	done := publishOnFirstGet(f, nil)
	if _, err := Run(context.Background(), fx.input(f.client(t))); err != nil {
		t.Fatal(err)
	}
	if !*done || f.count("PATCH ") != 0 {
		t.Fatalf("re-read %t, writes %v", *done, f.writes())
	}
	if b := f.byTag(tag)[0].Body; b == "new notes" {
		t.Fatal("published release edited")
	}
}

// TestPublishedMeanwhileWithOtherContentIsNotRecorded: when a concurrent
// run published the draft with other content, this run's manifest does not
// record that release's URLs as its own.
func TestPublishedMeanwhileWithOtherContentIsNotRecorded(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
	publishOnFirstGet(f, func(rel *fakeRelease) {
		for _, a := range rel.assets {
			if a.Name == checksumName {
				d := "sha256:" + sum("other")
				a.Digest = &d
			}
		}
	})
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "not this build's") {
		t.Fatalf("err = %v", err)
	}
	if f.count("PATCH ") != 0 || fx.downloadURL(checksumName) != "" {
		t.Fatalf("writes %v, url %q", f.writes(), fx.downloadURL(checksumName))
	}
}
