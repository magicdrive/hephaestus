package prgate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/prgate"
	fake "github.com/magicdrive/hephaestus/internal/prgate/prgatetest"
)

func merge(t *testing.T, s *fake.Server, cfg prgate.Config, method string) *prgate.MergeOutcome {
	t.Helper()
	out, err := newGate(t, s).Merge(context.Background(), cfg, method)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMerge_PinsTheHeadAndVerifiesTheParents(t *testing.T) {
	s := fake.New(t)
	out := merge(t, s, config(), "merge")
	if !out.Merged || !out.Verified || out.Problem != "" {
		t.Fatalf("outcome %+v", out)
	}
	if s.MergeCalls != 1 {
		t.Fatalf("merge requests = %d, want 1", s.MergeCalls)
	}
	if b := s.MergeBodies[0]; b["sha"] != fake.Head || b["merge_method"] != "merge" {
		t.Errorf("merge body %v: must pin sha to the expected head", b)
	}
	if out.MergeCommitSHA != fake.Merge {
		t.Errorf("merge commit %s", out.MergeCommitSHA)
	}
}

func TestMerge_Squash(t *testing.T) {
	s := fake.New(t)
	s.MergeParents = []string{fake.Base}
	out := merge(t, s, config(), "squash")
	if !out.Verified || s.MergeBodies[0]["merge_method"] != "squash" {
		t.Fatalf("outcome %+v body %v", out, s.MergeBodies)
	}
}

func TestMerge_NothingIsSentWhenTheGateFails(t *testing.T) {
	for name, mutate := range map[string]func(*fake.Server){
		"draft":          func(s *fake.Server) { s.Pull.Draft = true },
		"ci-gate failed": func(s *fake.Server) { replaceRun(s, done(20, "ci-gate", "failure")) },
		"behind":         func(s *fake.Server) { s.Compare.Behind = 1 },
		"rules 500":      func(s *fake.Server) { s.Errors["rules"] = 500 },
	} {
		t.Run(name, func(t *testing.T) {
			s := fake.New(t)
			mutate(s)
			out := merge(t, s, config(), "merge")
			if out.Report.Passed || out.Attempted || out.Merged || s.MergeCalls != 0 || len(s.Writes) != 0 {
				t.Fatalf("outcome %+v, writes %v", out, s.Writes)
			}
		})
	}
}

func TestMerge_FinalRecheck(t *testing.T) {
	tests := []struct {
		name   string
		change func(*fake.Server)
	}{
		{"head pushed after the evaluation", func(s *fake.Server) { s.Pull.HeadSHA = strings.Repeat("9", 40) }},
		{"converted to draft", func(s *fake.Server) { s.Pull.Draft = true }},
		{"retargeted", func(s *fake.Server) { s.Pull.BaseRef = "other" }},
		{"closed", func(s *fake.Server) { s.Pull.State = "closed" }},
		{"base moved", func(s *fake.Server) { s.BaseRefSHA = strings.Repeat("8", 40) }},
		{"pull unreadable", func(s *fake.Server) { s.Errors["pull"] = 502 }},
		{"base unreadable", func(s *fake.Server) { s.Errors["ref"] = 502 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fake.New(t)
			// Read 1 is the evaluation's; read 2 is the final re-check.
			s.OnPullRead = func(s *fake.Server, n int) {
				if n == 2 {
					tt.change(s)
				}
			}
			out := merge(t, s, config(), "merge")
			if !out.Report.Passed {
				t.Fatalf("evaluation failed:%s", dump(out.Report))
			}
			if out.Attempted || s.MergeCalls != 0 {
				t.Fatalf("merge sent despite %s: %+v", tt.name, out)
			}
			if !strings.Contains(out.Problem, "final re-check failed") {
				t.Errorf("problem %q", out.Problem)
			}
		})
	}
}

func TestMerge_Refused(t *testing.T) {
	for _, code := range []int{405, 409, 422, 403} {
		s := fake.New(t)
		s.MergeStatus = code
		out := merge(t, s, config(), "merge")
		if !out.Attempted || out.Merged || out.Verified || out.Unknown {
			t.Errorf("%d: outcome %+v", code, out)
		}
		if s.MergeCalls != 1 {
			t.Errorf("%d: merge requests = %d, want 1", code, s.MergeCalls)
		}
	}
}

func TestMerge_IsNeverRetried(t *testing.T) {
	s := fake.New(t)
	s.MergeStatus = 502
	out := merge(t, s, config(), "merge")
	if s.MergeCalls != 1 {
		t.Fatalf("merge requests = %d, want exactly 1", s.MergeCalls)
	}
	if out.Merged {
		t.Fatalf("outcome %+v", out)
	}
}

func TestMerge_LostResponseIsDetected(t *testing.T) {
	s := fake.New(t)
	s.MergeStatus = 502
	// The merge happened, but the response was lost.
	s.OnPullRead = func(s *fake.Server, n int) {
		if n == 3 {
			s.Pull.State, s.Pull.Merged, s.Pull.MergeCommitSHA = "closed", true, fake.Merge
		}
	}
	out := merge(t, s, config(), "merge")
	if !out.Merged || !out.Verified {
		t.Fatalf("outcome %+v", out)
	}
}

func TestMerge_BaseMovedAtTheLastMomentIsAnAlarm(t *testing.T) {
	s := fake.New(t)
	s.MergeParents = []string{strings.Repeat("8", 40), fake.Head}
	out := merge(t, s, config(), "merge")
	if !out.Merged || out.Verified || !strings.Contains(out.Problem, "base moved") {
		t.Fatalf("outcome %+v", out)
	}
}

func TestMerge_UnknownResultAfterTheRequest(t *testing.T) {
	s := fake.New(t)
	s.MergeStatus = 502
	s.OnPullRead = func(s *fake.Server, n int) {
		if n == 3 {
			s.Errors["pull"] = 502
		}
	}
	out := merge(t, s, config(), "merge")
	if !out.Unknown || out.Merged || out.Verified {
		t.Fatalf("outcome %+v", out)
	}
}

func TestMerge_MergeCommitUnreadable(t *testing.T) {
	s := fake.New(t)
	s.Errors["commit"] = 500
	out := merge(t, s, config(), "merge")
	if !out.Merged || out.Verified {
		t.Fatalf("outcome %+v", out)
	}
}

func TestMerge_RejectsRebase(t *testing.T) {
	s := fake.New(t)
	if _, err := newGate(t, s).Merge(context.Background(), config(), "rebase"); err == nil {
		t.Fatal("rebase accepted: its result cannot be verified by parents")
	}
	if s.PullReads != 0 {
		t.Error("API called")
	}
}
