package prgate_test

import (
	"fmt"
	"testing"

	"github.com/magicdrive/hephaestus/internal/prgate"
	fake "github.com/magicdrive/hephaestus/internal/prgate/prgatetest"
)

// TestEvaluate_BehindBaseNeverPasses is the merge gate's side of the tap
// race (docs/tap-version-guard.md): every check of a pull request is green,
// but against a base that has since moved (another release was merged), so
// the head is behind it. Whatever the compare status says, behind by one
// or more fails base.up_to_date and the gate, and -merge sends nothing:
// a green result of an older base is never used.
func TestEvaluate_BehindBaseNeverPasses(t *testing.T) {
	for _, behind := range []int{1, 2, 50} {
		for _, st := range []string{"behind", "diverged", "ahead", "identical"} {
			t.Run(fmt.Sprintf("%s by %d", st, behind), func(t *testing.T) {
				s := fake.New(t)
				s.Compare.Status, s.Compare.Behind = st, behind
				rep := evaluate(t, s, config())
				if rep.Passed || status(rep, "base.up_to_date") != prgate.Fail {
					t.Fatalf("passed=%v base.up_to_date=%q:%s", rep.Passed, status(rep, "base.up_to_date"), dump(rep))
				}
				s = fake.New(t)
				s.Compare.Status, s.Compare.Behind = st, behind
				if out := merge(t, s, config(), "merge"); out.Attempted || s.MergeCalls != 0 || len(s.Writes) != 0 {
					t.Fatalf("merged a pull request behind its base: %+v, writes %v", out, s.Writes)
				}
			})
		}
	}
	t.Run("up to date", func(t *testing.T) {
		s := fake.New(t)
		s.Compare.Status, s.Compare.Behind = "ahead", 0
		if rep := evaluate(t, s, config()); !rep.Passed || status(rep, "base.up_to_date") != prgate.Pass {
			t.Fatalf("gate failed:%s", dump(rep))
		}
	})
}
