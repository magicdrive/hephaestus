package tap

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// --- Downgrades through the merge order of open pull requests ---

// releaseInput is the tap input of release v<version> of tool, whose
// formula declares version.
func releaseInput(t *testing.T, f *fakeGitHub, version string, mutate ...func(*config.Brew)) Input {
	t.Helper()
	root := t.TempDir()
	content := formulaWith(version)
	rel := "dist/homebrew/Formula/tool.rb"
	if err := os.MkdirAll(filepath.Join(root, "dist/homebrew/Formula"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m := manifest.New("tool", version, "v"+version, "abc")
	m.Put(manifest.Artifact{Kind: manifest.KindFormula, ID: "tool", Name: "tool.rb", Path: rel, SHA256: sum(content), Size: int64(len(content)), Source: manifest.Source{Stage: "homebrew"}})
	cfg := &config.Config{Root: root, Project: config.Project{Name: "tool"},
		Brews: []config.Brew{{Name: "tool", Directory: "Formula", Repository: config.BrewRepository{Owner: "acme", Name: "homebrew-tap"}}}}
	for _, fn := range mutate {
		fn(&cfg.Brews[0])
	}
	return Input{Config: cfg, Manifest: m, Client: f.client(t, token),
		Vars: tmpl.Vars{Project: "tool", ProjectName: "tool", Version: version, Tag: "v" + version},
		Log:  testLog{t}}
}

func publishVersion(t *testing.T, f *fakeGitHub, version string, mutate ...func(*config.Brew)) Result {
	t.Helper()
	return one(t)(Run(context.Background(), releaseInput(t, f, version, mutate...)))
}

func allowDowngrade(b *config.Brew) { b.AllowDowngrade = true }

func warnings(r Result) string { return strings.Join(r.Warnings, "\n") }

// TestVersionTransitions: the base branch's version against the one being
// published, by SemVer precedence (not as strings).
func TestVersionTransitions(t *testing.T) {
	for _, tc := range []struct {
		base, publish string
		want          string
	}{
		{"1.2.0", "1.3.0", StatusUpdated},
		{"1.3.0", "1.2.0", StatusSkipped},
		{"1.3.0-rc.1", "1.3.0", StatusUpdated},
		{"1.3.0", "1.3.0-rc.1", StatusSkipped},
		{"1.3.0-rc.1", "1.3.0-rc.2", StatusUpdated},
		{"1.3.0-rc.10", "1.3.0-rc.9", StatusSkipped},
		{"1.9.0", "1.10.0", StatusUpdated},
		{"1.10.0", "1.9.0", StatusSkipped},
		{"1.3.0+build.1", "1.3.0+build.2", StatusUpdated}, // build metadata: same precedence
	} {
		t.Run(tc.base+"->"+tc.publish, func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith(tc.base)})
			r := publishVersion(t, f, tc.publish)
			if r.Status != tc.want {
				t.Fatalf("status %s, want %s: %+v", r.Status, tc.want, r)
			}
			if tc.want == StatusSkipped && (len(f.writes()) != 0 || len(f.pullList()) != 0) {
				t.Fatalf("a downgrade wrote %v", f.writes())
			}
		})
	}
}

// TestNewerOpenPullBlocksOlder: v1.3.0's pull request is open (not merged);
// v1.2.0 must not open a pull request that, merged after it, downgrades the
// formula.
func TestNewerOpenPullBlocksOlder(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.1.0"), "README.md": "tap"})
		if r := publishVersion(t, f, "1.3.0"); r.Status != StatusUpdated || len(r.Warnings) != 0 {
			t.Fatalf("v1.3.0: %+v", r)
		}
		f.resetLog()
		in := releaseInput(t, f, "1.2.0")
		in.DryRun = dry
		r := one(t)(Run(context.Background(), in))
		if r.Status != StatusSkipped || !r.NoOp || !strings.Contains(r.Reason, "pull/1") || !strings.Contains(r.Reason, "1.3.0") ||
			!strings.Contains(warnings(r), "proposes a newer version (1.3.0); not proposing 1.2.0") {
			t.Fatalf("dry=%t v1.2.0: %+v", dry, r)
		}
		if w := f.writes(); len(w) != 0 || f.hasBranch(tapRepo, "tool-1.2.0") || len(f.pullList()) != 1 {
			t.Fatalf("dry=%t: writes %v pulls %+v", dry, w, f.pullList())
		}
	}
}

// TestOlderOpenPullIsNamed: v1.2.0's pull request is open when v1.3.0 is
// published: the new one is opened, warns, and its body names the older one
// and the merge order. A re-run of v1.2.0 is then skipped.
func TestOlderOpenPullIsNamed(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.1.0")})
	if r := publishVersion(t, f, "1.2.0"); r.Status != StatusUpdated {
		t.Fatalf("v1.2.0: %+v", r)
	}
	r := publishVersion(t, f, "1.3.0")
	if r.Status != StatusUpdated || r.PullRequestURL != "https://github.example/acme/homebrew-tap/pull/2" ||
		!strings.Contains(warnings(r), "pull/1 proposes older version 1.2.0 of Formula/tool.rb: close it") {
		t.Fatalf("v1.3.0: %+v", r)
	}
	body := f.pullList()[1].Body
	for _, want := range []string{"version 1.3.0", "do not merge a pull request with an older version of Formula/tool.rb after this one", "- #1 (1.2.0)", "<!-- hephaestus-formula: Formula/tool.rb 1.3.0 -->"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	// The v1.2.0 workflow is re-run: its own pull request #1 exists, but
	// #2 is newer.
	if r := publishVersion(t, f, "1.2.0"); r.Status != StatusSkipped || !strings.Contains(r.Reason, "pull/2") {
		t.Fatalf("v1.2.0 re-run: %+v", r)
	}
	// An intentional rollback is explicit.
	if r := publishVersion(t, f, "1.2.0", allowDowngrade); r.Status != StatusUnchanged && r.Status != StatusUpdated ||
		!strings.Contains(warnings(r), "merging this one after it downgrades the formula (allow_downgrade is set)") {
		t.Fatalf("v1.2.0 allow_downgrade: %+v", r)
	}
}

// TestOpenPullsCompareBySemver: prerelease, build metadata and numeric
// ordering of open pull requests, and pull requests that do not change the
// formula.
func TestOpenPullsCompareBySemver(t *testing.T) {
	for _, tc := range []struct {
		name, rival, publish string
		skipped              bool
		warn                 string
	}{
		{"rc then final", "1.3.0-rc.1", "1.3.0", false, "older version 1.3.0-rc.1"},
		{"final then rc", "1.3.0", "1.3.0-rc.1", true, "newer version (1.3.0)"},
		{"numeric, not string", "1.10.0", "1.9.0", true, "newer version (1.10.0)"},
		{"numeric, not string (older)", "1.9.0", "1.10.0", false, "older version 1.9.0"},
		{"same precedence", "1.3.0+build.7", "1.3.0", false, "also proposes version 1.3.0+build.7"},
		{"not comparable", "2024.1", "1.3.0", false, "cannot be compared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.0.0")})
			f.openRivalPull(tapRepo, "main", "rival", formulaPath, formulaWith(tc.rival))
			r := publishVersion(t, f, tc.publish)
			if (r.Status == StatusSkipped) != tc.skipped || !strings.Contains(warnings(r), tc.warn) {
				t.Fatalf("%+v", r)
			}
		})
	}
	t.Run("pull request not changing the formula", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.0.0")})
		f.openRivalPull(tapRepo, "main", "docs", "README.md", "docs")
		f.openRivalPull(tapRepo, "main", "other", "Formula/other.rb", formulaWith("9.0.0"))
		if r := publishVersion(t, f, "1.1.0"); r.Status != StatusUpdated || len(r.Warnings) != 0 {
			t.Fatalf("%+v", r)
		}
	})
}

// TestStaleBase: v1.2.0's branch was created from an old base; v1.3.0 was
// merged since. A re-run of v1.2.0 compares with the moved base, not with
// its branch's merge base.
func TestStaleBase(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.1.0")})
	publishVersion(t, f, "1.2.0")
	publishVersion(t, f, "1.3.0")
	f.mergeBranch(tapRepo, "main", "tool-1.3.0")
	f.resetLog()
	r := publishVersion(t, f, "1.2.0")
	if r.Status != StatusSkipped || !strings.Contains(r.Reason, "the tap already has version 1.3.0") || len(f.writes()) != 0 {
		t.Fatalf("%+v writes %v", r, f.writes())
	}
}

// TestRacingReleases: another release opens its pull request while this one
// is between its check and its own pull request. The scan after opening
// reports it, whichever is newer.
func TestRacingReleases(t *testing.T) {
	for _, tc := range []struct{ other, publish, warn string }{
		{"1.3.0", "1.2.0", "(opened meanwhile) proposes version 1.3.0 of Formula/tool.rb, newer than 1.2.0: do not merge Formula/tool.rb after it; close this one"},
		{"1.2.0", "1.3.0", "proposes older version 1.2.0 of Formula/tool.rb: close it"},
	} {
		t.Run(tc.publish+" racing "+tc.other, func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.1.0")})
			done := false
			f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls") && !done {
					done = true
					f.openRivalPull(tapRepo, "main", "tool-"+tc.other, formulaPath, formulaWith(tc.other))
				}
				return false
			}
			r := publishVersion(t, f, tc.publish)
			if r.Status != StatusUpdated || !strings.Contains(warnings(r), tc.warn) {
				t.Fatalf("%+v", r)
			}
		})
	}
}
