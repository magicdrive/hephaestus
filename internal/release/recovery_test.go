package release

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Recovery: a run that fails after the draft is complete must leave a
// draft that a re-run publishes, without a second release, and a lost
// publish response must not publish twice or fail the run.

// isPublishPATCH reports whether r publishes a release (draft: false).
func isPublishPATCH(r *http.Request, body []byte) bool {
	if r.Method != http.MethodPatch || !strings.Contains(r.URL.Path, "/releases/") {
		return false
	}
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	return req["draft"] == false
}

func TestPublishFailureLeavesDraftAndRerunPublishes(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	down := true
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if down && isPublishPATCH(r, body) {
			reply(w, http.StatusInternalServerError, map[string]any{"message": "Server Error"})
			return true
		}
		return false
	}
	_, err := Run(context.Background(), fx.input(f.client(t)))
	if err == nil || !strings.Contains(err.Error(), "re-run to retry") {
		t.Fatalf("err = %v", err)
	}
	rels := f.byTag(tag)
	if len(rels) != 1 || !rels[0].Draft || len(rels[0].assets) != 3 {
		t.Fatalf("after the failure: %d releases %+v", len(rels), rels)
	}

	down = false
	f.resetLog()
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	rels = f.byTag(tag)
	if len(rels) != 1 || rels[0].Draft || !res.Published {
		t.Fatalf("after the re-run: %d releases, published %t", len(rels), res.Published)
	}
	// Only the publish: no create, no upload, no delete.
	for _, w := range f.writes() {
		if !strings.HasPrefix(w, "PATCH ") {
			t.Errorf("re-run wrote %s", w)
		}
	}
	wantStatuses(t, res, all(StatusUnchanged))
}

func TestPublishLostResponseIsNotPublishedTwice(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	lost := false
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !lost && isPublishPATCH(r, body) {
			// GitHub published the release, then the response was lost.
			lost = true
			f.mu.Lock()
			f.byTagLocked(tag).Draft = false
			f.mu.Unlock()
			reply(w, http.StatusBadGateway, map[string]any{"message": "Bad Gateway"})
			return true
		}
		return false
	}
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if rels := f.byTag(tag); len(rels) != 1 || rels[0].Draft || !res.Published {
		t.Fatalf("%d releases %+v", len(rels), res)
	}
	f.resetLog()
	res, err = Run(context.Background(), fx.input(f.client(t)))
	if err != nil || !res.NoOp || len(f.writes()) != 0 {
		t.Fatalf("re-run: err %v, no-op %t, writes %v", err, res.NoOp, f.writes())
	}
}

// TestImmutableReleaseRerunWritesNothing: a re-run against a complete
// immutable release, even with different notes configured, writes nothing:
// no asset, no notes, no publish.
func TestImmutableReleaseRerunWritesNothing(t *testing.T) {
	f := newFakeGitHub(t)
	fx := newFixture(t)
	fx.gh().Notes = "new notes"
	rel := f.addRelease(tag, false, true, map[string]string{linuxName: defaultFiles[linuxName], darwinName: defaultFiles[darwinName], checksumName: defaultFiles[checksumName]})
	rel.Body = "old notes"
	res, err := Run(context.Background(), fx.input(f.client(t)))
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("wrote to an immutable release: %v", w)
	}
	wantStatuses(t, res, all(StatusUnchanged))
	if got := f.byTag(tag)[0]; got.Body != "old notes" || got.Draft || len(got.assets) != 3 || !res.NoOp {
		t.Errorf("release %+v", got)
	}
}
