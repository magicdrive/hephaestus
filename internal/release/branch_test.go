package release

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The release branch check (release.github.branches): the commit being
// released must be in the history of a release branch, or nothing is
// created or uploaded. The fake answers git/ref/heads/<branch> and
// compare/<head>...<commit> from branches and inBranch.

const (
	mainHead  = "1111111111111111111111111111111111111111"
	maintHead = "2222222222222222222222222222222222222222"
	onMain    = "3333333333333333333333333333333333333333"
	onMaint   = "4444444444444444444444444444444444444444"
	onFeature = "5555555555555555555555555555555555555555"
	tagObject = "6666666666666666666666666666666666666666"
)

// branchFake is a repository with main (default), v1.x and feature, the
// commits onMain, onMaint and onFeature in the history of each, and the
// tag v1.0.0 at commit (annotated when annotated is set).
func branchFake(t *testing.T, commit string, annotated bool) *fakeGitHub {
	f := newFakeGitHub(t)
	f.branches = map[string]string{"main": mainHead, "v1.x": maintHead, "feature": onFeature}
	history := map[string][]string{"main": {onMain}, "v1.x": {onMaint}, "feature": {onMain}}
	f.inBranch = func(b, c string) bool {
		for _, x := range history[b] {
			if x == c {
				return true
			}
		}
		return false
	}
	if commit != "" {
		f.tags = map[string]string{tag: commit}
		if annotated {
			f.tags[tag] = tagObject
			f.tagObjects = map[string]string{tagObject: commit}
		}
	}
	return f
}

// runBranch runs a release of v1.0.0 built from commit with the release
// branches given, and returns the log and the error.
func runBranch(t *testing.T, f *fakeGitHub, commit string, branches ...string) (string, error) {
	t.Helper()
	fx := newFixture(t)
	fx.gh().Branches = branches
	lb := &logBuf{t: t}
	in := fx.input(f.client(t))
	in.Vars.Commit = commit
	in.Log = lb
	_, err := Run(context.Background(), in)
	return lb.String(), err
}

func TestReleaseBranch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		commit    string // the tag's commit and the built commit
		annotated bool
		branches  []string
		wantErr   string // "" means the release is published
		wantLog   string
	}{
		{"default branch, lightweight tag", onMain, false, nil, "", "tag v1.0.0 (" + onMain + ") is in the history of main"},
		{"default branch, annotated tag (peeled)", onMain, true, nil, "", "is in the history of main"},
		{"default branch head itself", mainHead, false, nil, "", "is in the history of main"},
		{"commit on no branch", "7777777777777777777777777777777777777777", false, nil, "is not in the history of a release branch (main)", ""},
		{"branch not allowed", onFeature, false, nil, "tag v1.0.0 (" + onFeature + ") is not in the history of a release branch (main)", ""},
		{"maintenance branch not allowed", onMaint, false, nil, "is not in the history of a release branch (main)", ""},
		{"allowed maintenance branch", onMaint, false, []string{"main", "v1.x"}, "", "is in the history of v1.x"},
		{"allowed maintenance branch, annotated", onMaint, true, []string{"main", "v1.x"}, "", "is in the history of v1.x"},
		{"only the maintenance branch allowed", onMain, false, []string{"v1.x"}, "is not in the history of a release branch (v1.x)", ""},
		{"allowed branch missing", onMaint, false, []string{"main", "v2.x"}, "cannot read release branch v2.x of acme/tool", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := branchFake(t, tc.commit, tc.annotated)
			log, err := runBranch(t, f, tc.commit, tc.branches...)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(log, tc.wantLog) {
					t.Fatalf("log lacks %q:\n%s", tc.wantLog, log)
				}
				if len(f.writes()) == 0 {
					t.Fatal("nothing was published")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was created or uploaded") {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("wrote although the release branch check failed: %v", w)
			}
		})
	}
}

// A published release re-run (recovery) is checked too: the tag decides.
func TestReleaseBranchPublishedRerun(t *testing.T) {
	f := branchFake(t, onFeature, false)
	f.addRelease(tag, false, false, nil)
	_, err := runBranch(t, f, onFeature)
	if err == nil || !strings.Contains(err.Error(), "is not in the history of a release branch") {
		t.Fatalf("err = %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("wrote: %v", w)
	}
}

// Before the tag exists GitHub creates it at target_commitish (configured,
// else the built commit), or at the default branch's head without one:
// that is the commit checked.
func TestReleaseBranchWithoutTag(t *testing.T) {
	for _, tc := range []struct {
		name, commit, target string
		branches             []string
		wantErr              string
	}{
		{"built commit on main", onMain, "", nil, ""},
		{"built commit on a feature branch", onFeature, "", nil, "target_commitish " + onFeature + " (" + onFeature + ") is not in the history of a release branch (main)"},
		{"configured target on main", "", onMain, nil, ""},
		{"configured target off main", onMain, onFeature, nil, "target_commitish " + onFeature},
		{"no commit, no target: the default branch head", "", "", nil, ""},
		{"no commit, no target, the default branch not allowed", "", "", []string{"v1.x"}, "GitHub would create it at the head of the default branch main, which is not a release branch (v1.x)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := branchFake(t, "", false)
			fx := newFixture(t)
			fx.gh().Branches = tc.branches
			fx.gh().TargetCommitish = tc.target
			in := fx.input(f.client(t))
			in.Vars.Commit = tc.commit
			in.Log = &logBuf{t: t}
			_, err := Run(context.Background(), in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("wrote: %v", w)
			}
		})
	}
}

// Anything that keeps the check from answering yes or no stops the release
// before any write: API errors on each request, answers that are neither,
// a ref that is not the branch.
func TestReleaseBranchFailsClosed(t *testing.T) {
	type answer struct {
		status int
		body   map[string]any
	}
	for _, tc := range []struct {
		name    string
		path    string // the GET path answered with a
		a       answer
		wantErr string
	}{
		{"default branch unreadable", "/repos/acme/tool", answer{403, map[string]any{"message": "Resource not accessible by integration"}}, "cannot read the default branch of acme/tool"},
		{"no default branch", "/repos/acme/tool", answer{200, map[string]any{"full_name": "acme/tool"}}, "reports no default branch"},
		{"branch unreadable", "/repos/acme/tool/git/ref/heads/main", answer{500, map[string]any{"message": "boom"}}, "cannot read release branch main"},
		{"branch ref is another ref", "/repos/acme/tool/git/ref/heads/main", answer{200, map[string]any{"ref": "refs/heads/main-old", "object": map[string]any{"sha": mainHead, "type": "commit"}}}, `GitHub answered ref "refs/heads/main-old"`},
		{"branch ref names a tag object", "/repos/acme/tool/git/ref/heads/main", answer{200, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": mainHead, "type": "tag"}}}, "tag"},
		{"branch head not a SHA", "/repos/acme/tool/git/ref/heads/main", answer{200, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": "main", "type": "commit"}}}, `"main"`},
		{"compare fails", "/repos/acme/tool/compare/" + mainHead + "..." + onMain, answer{404, map[string]any{"message": "Not Found"}}, "cannot compare tag v1.0.0 with release branch main"},
		{"compare answers an unknown status", "/repos/acme/tool/compare/" + mainHead + "..." + onMain, answer{200, map[string]any{"status": "unknown"}}, `status "unknown"`},
		{"compare says behind but something is ahead", "/repos/acme/tool/compare/" + mainHead + "..." + onMain, answer{200, map[string]any{"status": "behind", "ahead_by": 2}}, `status "behind" with 2 ahead`},
		{"compare says ahead", "/repos/acme/tool/compare/" + mainHead + "..." + onMain, answer{200, map[string]any{"status": "ahead", "ahead_by": 1}}, "is not in the history of a release branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := branchFake(t, onMain, false)
			f.hook = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
				if r.Method == "GET" && r.URL.Path == tc.path {
					reply(w, tc.a.status, tc.a.body)
					return true
				}
				return false
			}
			_, err := runBranch(t, f, onMain)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if f.count("GET "+tc.path) == 0 {
				t.Fatalf("%s was never requested", tc.path)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("wrote: %v", w)
			}
		})
	}
}

// A dry run without a token reads nothing: it says the branch was not
// verified. With a token the dry run verifies it like a release.
func TestReleaseBranchDryRun(t *testing.T) {
	fx := newFixture(t)
	lb := &logBuf{t: t}
	in := fx.input(nil)
	in.DryRun = true
	in.Log = lb
	if _, err := Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lb.String(), "the release branch of v1.0.0 was not verified") {
		t.Fatalf("no warning:\n%s", lb)
	}

	f := branchFake(t, onFeature, false)
	fx = newFixture(t)
	in = fx.input(f.client(t))
	in.DryRun = true
	in.Vars.Commit = onFeature
	in.Log = &logBuf{t: t}
	if _, err := Run(context.Background(), in); err == nil || !strings.Contains(err.Error(), "is not in the history of a release branch") {
		t.Fatalf("dry run err = %v", err)
	}
}
