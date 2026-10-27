package prgate

import "testing"

func TestFinish_EveryConditionMustBeReported(t *testing.T) {
	all := func(skip string) *Report {
		r := &Report{}
		for _, id := range FindingIDs {
			if id != skip {
				r.add(id, Pass, "ok")
			}
		}
		r.finish()
		return r
	}
	if !all("").Passed {
		t.Fatal("a report with every condition passing failed")
	}
	for _, id := range FindingIDs {
		if all(id).Passed {
			t.Errorf("passed without a %s finding", id)
		}
	}
	r := &Report{}
	r.finish()
	if r.Passed {
		t.Error("an empty report passed")
	}
}

func TestFinish_AnyNonPassFails(t *testing.T) {
	// A second finding for a condition that already passed, or a finding
	// for a condition that is not listed.
	for _, extra := range []string{"checks.required", "unlisted"} {
		for _, st := range []Status{Fail, Error, Status("")} {
			r := &Report{}
			for _, id := range FindingIDs {
				r.add(id, Pass, "ok")
			}
			r.add(extra, st, "x")
			r.finish()
			if r.Passed {
				t.Errorf("passed with a %q %s finding", st, extra)
			}
		}
	}
}
