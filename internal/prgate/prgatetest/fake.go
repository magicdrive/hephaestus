// Package prgatetest is a fake GitHub API (httptest) for the merge gate's
// tests: a pull request that passes every condition by default, with
// fields to break one condition at a time and to inject API failures.
package prgatetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Fixed values of the default pull request.
const (
	Owner   = "o"
	Repo    = "r"
	Number  = 7
	Head    = "1111111111111111111111111111111111111111"
	Base    = "2222222222222222222222222222222222222222"
	Merge   = "3333333333333333333333333333333333333333"
	BaseRef = "main"
	// ActionsApp is the app ID of GitHub Actions.
	ActionsApp = 15368
)

// Run is a check run.
type Run struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	App        int64
}

// WorkflowRun is a workflow run.
type WorkflowRun struct {
	ID, WorkflowID int64
	Name, Event    string
	Status         string
	Conclusion     string
}

// Status is a commit status.
type Status struct{ Context, State string }

// Pull is the state of the pull request the server reports.
type Pull struct {
	State          string
	Draft, Merged  bool
	Mergeable      *bool // nil: still computing
	MergeableState string
	HeadSHA        string
	BaseRef        string
	MergeCommitSHA string
}

// Server is the fake API. Lock Mu to change fields while requests run.
type Server struct {
	Mu sync.Mutex
	*httptest.Server

	Pull Pull
	// PullSequence, when set, is served for the first reads instead of
	// Pull, one per read (mergeability still being computed).
	PullSequence []Pull
	// OnPullRead runs before the n-th read (1-based) of the pull request
	// is answered; it may change the state (a push during the gate).
	OnPullRead  func(s *Server, n int)
	BaseRefSHA  string
	BaseRefType string
	Compare     struct {
		Status        string
		Ahead, Behind int
	}
	// Rules is the raw JSON of GET rules/branches/{branch}.
	Rules string
	// Branch is the raw JSON of GET branches/{branch}.
	Branch      string
	CheckRuns   []Run
	Statuses    []Status
	StatusTotal int // -1: len(Statuses)
	Workflows   []WorkflowRun
	// Errors maps an endpoint name (pull, ref, compare, rules, branch,
	// check-runs, status, runs, commit, merge) to the HTTP status to
	// answer with; 403 is a rate limit.
	Errors map[string]int
	// BadJSON names endpoints answering with invalid JSON.
	BadJSON map[string]bool

	// Merge behaviour: MergeStatus answers the PUT (200 merges);
	// MergeParents are the parents of the merge commit.
	MergeStatus  int
	MergeParents []string

	// Observations.
	PullReads   int
	MergeCalls  int
	MergeBodies []map[string]string
	Writes      []string // method and path of every non-GET request
}

func ptr[T any](v T) *T { return &v }

// New starts a server reporting a pull request that passes every
// condition. It is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{
		Pull:        Pull{State: "open", Mergeable: ptr(true), MergeableState: "clean", HeadSHA: Head, BaseRef: BaseRef},
		BaseRefSHA:  Base,
		BaseRefType: "commit",
		Rules:       `[{"type":"pull_request","ruleset_id":1,"parameters":{}},{"type":"required_status_checks","ruleset_id":1,"parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"ci-gate","integration_id":15368},{"context":"test","integration_id":15368}]}}]`,
		Branch:      `{"name":"main","protected":true,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`,
		CheckRuns: []Run{
			{ID: 10, Name: "test", Status: "completed", Conclusion: "success", App: ActionsApp},
			{ID: 11, Name: "lint", Status: "completed", Conclusion: "success", App: ActionsApp},
			{ID: 12, Name: "extended", Status: "completed", Conclusion: "skipped", App: ActionsApp},
			{ID: 20, Name: "ci-gate", Status: "completed", Conclusion: "success", App: ActionsApp},
		},
		StatusTotal:  -1,
		Workflows:    []WorkflowRun{{ID: 100, WorkflowID: 1, Name: "CI", Event: "pull_request", Status: "completed", Conclusion: "success"}},
		Errors:       map[string]int{},
		BadJSON:      map[string]bool{},
		MergeStatus:  200,
		MergeParents: []string{Base, Head},
	}
	s.Compare.Status, s.Compare.Ahead = "ahead", 2
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if raw, ok := v.(string); ok {
		_, _ = w.Write([]byte(raw))
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) pullJSON(p Pull) map[string]any {
	m := map[string]any{
		"number": Number, "state": p.State, "draft": p.Draft, "merged": p.Merged,
		"mergeable": p.Mergeable, "mergeable_state": p.MergeableState,
		"html_url": "https://github.example/o/r/pull/7",
		"head":     map[string]any{"ref": "feature", "sha": p.HeadSHA, "repo": map[string]any{"full_name": "o/r"}},
		"base":     map[string]any{"ref": p.BaseRef, "sha": Base},
	}
	if p.MergeCommitSHA != "" {
		m["merge_commit_sha"] = p.MergeCommitSHA
	}
	return m
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if r.Method != http.MethodGet {
		s.Writes = append(s.Writes, r.Method+" "+r.URL.Path)
	}
	prefix := "/repos/" + Owner + "/" + Repo + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		write(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, prefix)
	var name string
	switch {
	case p == "pulls/7" && r.Method == http.MethodGet:
		name = "pull"
	case p == "pulls/7/merge" && r.Method == http.MethodPut:
		name = "merge"
	case p == "git/ref/heads/"+BaseRef:
		name = "ref"
	case p == "compare/"+Base+"..."+Head:
		name = "compare"
	case p == "rules/branches/"+BaseRef:
		name = "rules"
	case p == "branches/"+BaseRef:
		name = "branch"
	case p == "commits/"+Head+"/check-runs":
		name = "check-runs"
	case p == "commits/"+Head+"/status":
		name = "status"
	case p == "actions/runs" && r.URL.Query().Get("head_sha") == Head:
		name = "runs"
	case p == "commits/"+Merge:
		name = "commit"
	default:
		write(w, 404, map[string]string{"message": "Not Found: " + r.Method + " " + p})
		return
	}
	if name == "pull" {
		s.PullReads++
		if s.OnPullRead != nil {
			s.OnPullRead(s, s.PullReads)
		}
	}
	if name == "merge" {
		s.MergeCalls++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.MergeBodies = append(s.MergeBodies, body)
	}
	if code := s.Errors[name]; code != 0 {
		if code == 403 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1")
			write(w, 403, map[string]string{"message": "API rate limit exceeded"})
			return
		}
		write(w, code, map[string]string{"message": http.StatusText(code)})
		return
	}
	if s.BadJSON[name] {
		write(w, 200, `{"broken":`)
		return
	}
	switch name {
	case "pull":
		pl := s.Pull
		if len(s.PullSequence) > 0 {
			pl, s.PullSequence = s.PullSequence[0], s.PullSequence[1:]
		}
		write(w, 200, s.pullJSON(pl))
	case "merge":
		if s.MergeStatus != 200 {
			write(w, s.MergeStatus, map[string]string{"message": "merge refused"})
			return
		}
		if s.Pull.HeadSHA != Head {
			write(w, 409, map[string]string{"message": "Head branch was modified"})
			return
		}
		s.Pull.State, s.Pull.Merged, s.Pull.MergeCommitSHA = "closed", true, Merge
		write(w, 200, map[string]any{"sha": Merge, "merged": true, "message": "Pull Request successfully merged"})
	case "ref":
		write(w, 200, map[string]any{"ref": "refs/heads/" + BaseRef, "object": map[string]string{"sha": s.BaseRefSHA, "type": s.BaseRefType}})
	case "compare":
		write(w, 200, map[string]any{"status": s.Compare.Status, "ahead_by": s.Compare.Ahead, "behind_by": s.Compare.Behind})
	case "rules":
		write(w, 200, s.Rules)
	case "branch":
		write(w, 200, s.Branch)
	case "check-runs":
		var runs []map[string]any
		for _, cr := range s.CheckRuns {
			m := map[string]any{"id": cr.ID, "name": cr.Name, "status": cr.Status, "conclusion": nil}
			if cr.Conclusion != "" {
				m["conclusion"] = cr.Conclusion
			}
			if cr.App != 0 {
				m["app"] = map[string]any{"id": cr.App, "slug": "app"}
			}
			runs = append(runs, m)
		}
		write(w, 200, map[string]any{"total_count": len(runs), "check_runs": runs})
	case "status":
		var sts []map[string]string
		for _, st := range s.Statuses {
			sts = append(sts, map[string]string{"context": st.Context, "state": st.State})
		}
		total := s.StatusTotal
		if total < 0 {
			total = len(sts)
		}
		write(w, 200, map[string]any{"state": "success", "total_count": total, "statuses": sts})
	case "runs":
		var runs []map[string]any
		for _, wr := range s.Workflows {
			m := map[string]any{"id": wr.ID, "workflow_id": wr.WorkflowID, "name": wr.Name, "event": wr.Event, "status": wr.Status, "conclusion": nil, "head_sha": Head, "run_attempt": 1}
			if wr.Conclusion != "" {
				m["conclusion"] = wr.Conclusion
			}
			runs = append(runs, m)
		}
		write(w, 200, map[string]any{"total_count": len(runs), "workflow_runs": runs})
	case "commit":
		var parents []map[string]string
		for _, sha := range s.MergeParents {
			parents = append(parents, map[string]string{"sha": sha})
		}
		write(w, 200, map[string]any{"sha": Merge, "parents": parents})
	}
}
