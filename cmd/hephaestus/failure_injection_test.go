package hephaestus

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Failure injection: "hephaestus run" (every stage) against the fake GitHub
// of pipeline_e2e_test.go, with one kind of request failing. Whatever
// fails, the run must not report a failure as a success, and must not leave
// a release that is public but incomplete; a re-run without the fault must
// finish the release without duplicating anything.

// e2eFault fails the requests to repo whose method and path match.
type e2eFault struct {
	repo, method string
	path         *regexp.Regexp
	// status is the HTTP status of the failure; 0 drops the connection
	// (a network failure or timeout as the client sees it).
	status int
	// after lets the request take effect and loses its response instead.
	after bool
	// skip lets the first skip matching requests through.
	skip int
	// times is how many matching requests fail after skip; < 0 is all.
	times int
	hits  int
}

// hit reports whether r is to fail (f.mu held).
func (fl *e2eFault) hit(repo string, r *http.Request) bool {
	if repo != fl.repo || r.Method != fl.method || !fl.path.MatchString(r.URL.Path) {
		return false
	}
	fl.hits++
	if fl.hits <= fl.skip {
		return false
	}
	return fl.times < 0 || fl.hits-fl.skip <= fl.times
}

func (fl *e2eFault) fail(w http.ResponseWriter) {
	if fl.status == 0 {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	switch fl.status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		// Retry at once: the client's waits are not under test here.
		w.Header().Set("Retry-After", "0")
	}
	e2eReply(w, fl.status, map[string]any{"message": http.StatusText(fl.status) + " (injected)"})
}

// lastOutput returns the last value of output name in $GITHUB_OUTPUT.
func lastOutput(t *testing.T, name string) string {
	t.Helper()
	data, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	lines := strings.Split(string(data), "\n")
	val := ""
	for i := 0; i+1 < len(lines); i++ {
		if k, delim, ok := strings.Cut(lines[i], "<<"); ok && k == name {
			var v []string
			for j := i + 1; j < len(lines) && lines[j] != delim; j++ {
				v = append(v, lines[j])
			}
			val = strings.Join(v, "\n")
		}
	}
	return val
}

// runFresh runs every stage of v1.0.0 with empty step outputs and summary.
func (e *e2eEnv) runFresh() (int, string) {
	e.t.Helper()
	for _, k := range []string{"GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"} {
		if err := os.WriteFile(os.Getenv(k), nil, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	return e.run("v1.0.0", "")
}

// checkOutcome checks the invariants that hold whatever failed: the exit
// code, the status output and the summary agree; at most one release and
// one pull request exist; a release that is public has every asset, byte
// for byte; and a success means the release is public and the formula is
// proposed.
func (e *e2eEnv) checkOutcome(code int, log string) {
	t := e.t
	t.Helper()
	status := lastOutput(t, "status")
	summary, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
	switch {
	case code == 0 && status != "published" && status != "no-op":
		t.Errorf("exit 0 with status %q\n%s", status, log)
	case code != 0 && status != "failed":
		t.Errorf("exit %d with status %q (want failed)\n%s", code, status, log)
	case code != 0 && !strings.Contains(string(summary), "failed"):
		t.Errorf("exit %d but the step summary does not say failed:\n%s", code, summary)
	case code != 0 && lastOutput(t, "no-op") != "false":
		t.Errorf("exit %d with no-op %q", code, lastOutput(t, "no-op"))
	}
	want := e.assets()
	rels := e.gh.releaseList()
	if len(rels) > 1 {
		t.Errorf("%d releases (duplicates): %+v", len(rels), rels)
	}
	public := false
	for _, rel := range rels {
		if rel.Draft {
			continue
		}
		public = true
		e.gh.mu.Lock()
		for _, r := range e.gh.releases {
			if r.ID != rel.ID {
				continue
			}
			got := map[string]string{}
			for _, a := range r.assets {
				got[a.Name] = string(a.data)
			}
			if len(got) != len(want) {
				t.Errorf("public release %d has %d assets, want %d\n%s", rel.ID, len(got), len(want), log)
			}
			for name, data := range want {
				if got[name] != string(data) {
					t.Errorf("public release %d: asset %s missing or different", rel.ID, name)
				}
			}
		}
		e.gh.mu.Unlock()
	}
	pulls := e.gh.pullList()
	if len(pulls) > 1 {
		t.Errorf("%d pull requests (duplicates): %+v", len(pulls), pulls)
	}
	if code == 0 {
		formula, ok := e.gh.tapFile("tool-1.0.0", "tool.rb")
		if !public || len(pulls) != 1 || !ok || !strings.Contains(formula, `version "1.0.0"`) {
			t.Errorf("exit 0 but release public=%t, pulls %+v, formula on the branch %t\n%s", public, pulls, ok, log)
		}
	}
}

func TestPipelineFailureInjection(t *testing.T) {
	type endpoint struct {
		name, repo, method, path string
		write                    bool
		skip                     int
	}
	endpoints := []endpoint{
		{"release-lookup", e2eReleaseRepo, "GET", `/releases/tags/`, false, 0},
		{"release-create", e2eReleaseRepo, "POST", `/releases$`, true, 0},
		{"asset-upload", e2eReleaseRepo, "POST", `/releases/\d+/assets$`, true, 0},
		// The second asset fails: one asset is on the draft already.
		{"partial-upload", e2eReleaseRepo, "POST", `/releases/\d+/assets$`, true, 1},
		{"release-publish", e2eReleaseRepo, "PATCH", `/releases/\d+$`, true, 0},
		{"tap-read", e2eTapRepo, "GET", `/contents/tool.rb$`, false, 0},
		{"tap-branch", e2eTapRepo, "POST", `/git/refs$`, true, 0},
		{"tap-commit", e2eTapRepo, "PUT", `/contents/`, true, 0},
		{"tap-pull", e2eTapRepo, "POST", `/pulls$`, true, 0},
	}
	type mode struct {
		name     string
		times    int
		after    bool
		statuses []int
	}
	// A fault on every attempt needs no connection drop of its own: the
	// client classifies it as a 5xx (retried, then failed), and only waits
	// longer between the attempts.
	modes := []mode{
		{"once", 1, false, []int{403, 404, 409, 422, 429, 500, 0}},
		{"always", -1, false, []int{403, 404, 429, 500}},
		{"lost-response", 1, true, []int{502, 0}},
	}

	for _, ep := range endpoints {
		for _, m := range modes {
			for _, status := range m.statuses {
				if m.after && !ep.write {
					continue // a lost response: only for writes
				}
				name := fmt.Sprintf("%s/%s/%d", ep.name, m.name, status)
				if status == 0 {
					name = fmt.Sprintf("%s/%s/connection-drop", ep.name, m.name)
				}
				t.Run(name, func(t *testing.T) {
					e := newE2E(t, "", "")
					fl := &e2eFault{repo: ep.repo, method: ep.method, path: regexp.MustCompile(ep.path), status: status, after: m.after, skip: ep.skip, times: m.times}
					e.gh.mu.Lock()
					e.gh.fault = fl
					e.gh.mu.Unlock()
					code, log := e.runFresh()
					e.checkOutcome(code, log)
					e.gh.mu.Lock()
					hits := fl.hits
					e.gh.fault = nil
					e.gh.mu.Unlock()
					if hits <= ep.skip {
						t.Fatalf("the fault was never triggered (%d matching requests)\n%s", hits, log)
					}
					// A fault that never clears on a request every release
					// needs fails the run: never a success.
					if m.times < 0 && code == 0 && (status != 404 || ep.write) {
						t.Errorf("exit 0 although every %s %s failed with %d\n%s", ep.method, ep.path, status, log)
					}
					t.Logf("outcome: exit %d, status %s", code, lastOutput(t, "status"))

					// Retry safety: a re-run without the fault completes the
					// release and the pull request, with no duplicates, and
					// does not replace the release a failed run created.
					var before int64
					if rels := e.gh.releaseList(); len(rels) == 1 {
						before = rels[0].ID
					}
					code, log = e.runFresh()
					if code != 0 {
						t.Fatalf("re-run without the fault: exit %d\n%s", code, log)
					}
					e.checkOutcome(code, log)
					if rels := e.gh.releaseList(); len(rels) != 1 || before != 0 && rels[0].ID != before {
						t.Errorf("after the re-run: releases %+v (before: id %d)", rels, before)
					}
				})
			}
		}
	}
}

// TestPipelineTagCollision: the tag already exists at another commit (a
// tag moved, or a duplicate tag): nothing is written anywhere, and the
// failure is reported as such.
func TestPipelineTagCollision(t *testing.T) {
	e := newE2E(t, "", "")
	e.gh.mu.Lock()
	e.gh.tags["v1.0.0"] = strings.Repeat("f", 40)
	e.gh.mu.Unlock()
	code, log := e.runFresh()
	e.checkOutcome(code, log)
	if code == 0 {
		t.Fatalf("exit %d\n%s", code, log)
	}
	if w := e.gh.requests(e2eReleaseRepo, true); len(w) != 0 {
		t.Errorf("writes to the release repository: %v", w)
	}
	if r := e.gh.requests(e2eTapRepo, false); len(r) != 0 {
		t.Errorf("the tap was touched: %v", r)
	}
}
