package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCompareCommits(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		writeJSON(w, 200, map[string]any{
			"status": "ahead", "ahead_by": 1, "behind_by": 0, "total_commits": 1,
			"files": []any{map[string]any{"filename": "Formula/tool.rb", "status": "modified"}},
		})
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	cmp, err := c.CompareCommits(context.Background(), "acme", "tap", "main", "bot:feature/tool-1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/acme/tap/compare/main...bot:feature/tool-1.0.0" {
		t.Fatalf("path %q", gotPath)
	}
	if cmp.Status != CompareAhead || cmp.AheadBy != 1 || len(cmp.Files) != 1 || cmp.Files[0].Filename != "Formula/tool.rb" {
		t.Fatalf("comparison %+v", cmp)
	}
	for _, bad := range [][2]string{{"", "x"}, {"a", ""}, {"a...b", "c"}, {"main", "../x"}} {
		if _, err := c.CompareCommits(context.Background(), "acme", "tap", bad[0], bad[1]); err == nil {
			t.Errorf("CompareCommits(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestRenameAsset(t *testing.T) {
	var log []string
	var renameBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log = append(log, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/releases/assets/9":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &renameBody)
			writeJSON(w, 200, map[string]any{"id": 9, "name": renameBody["name"], "state": "uploaded"})
		default:
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	ctx := context.Background()
	a, err := c.RenameReleaseAsset(ctx, "o", "r", 9, "tool.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "tool.tar.gz" || renameBody["name"] != "tool.tar.gz" {
		t.Fatalf("asset %+v body %v", a, renameBody)
	}
	if _, err := c.RenameReleaseAsset(ctx, "o", "r", 9, ""); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestGetTagAndAssetTimes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/git/tags/abc":
			writeJSON(w, 200, map[string]any{"sha": "abc", "tag": "v1", "object": map[string]any{"sha": "def", "type": "commit"}})
		case "/repos/o/r/releases/1/assets":
			writeJSON(w, 200, []any{map[string]any{"id": 2, "name": "a", "state": "starter", "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:05:05Z"}})
		default:
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	ctx := context.Background()
	tg, err := c.GetTag(ctx, "o", "r", "abc")
	if err != nil || tg.Object.SHA != "def" || tg.Object.Type != "commit" {
		t.Fatalf("tag %+v err %v", tg, err)
	}
	if _, err := c.GetTag(ctx, "o", "r", "../x"); err == nil {
		t.Fatal("invalid sha accepted")
	}
	as, err := c.ListReleaseAssets(ctx, "o", "r", 1)
	if err != nil || len(as) != 1 || as[0].UpdatedAt.Sub(as[0].CreatedAt) != time.Minute {
		t.Fatalf("assets %+v err %v", as, err)
	}
}
