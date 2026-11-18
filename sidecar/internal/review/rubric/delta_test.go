package rubric

import "testing"

// review-01a: the delta between two grades. Silence means healthy: an
// unchanged run has an empty delta. No concurrent access tests: Delta is
// pure.

func TestDelta_NilPreviousIsTheWholeReportMarkedFirstRun(t *testing.T) {
	r, rs := happyRubric()
	cur := Compute(r, rs)
	d := Delta(nil, cur)
	if !d.FirstRun || d.RubricChanged || d.Empty() || len(d.Dimensions) != 5 {
		t.Fatalf("first run = %+v, want every dimension, marked first run", d)
	}
	if d.ToOverall != "D" || d.FromOverall != "" {
		t.Fatalf("first run overall %q -> %q, want unknown -> D", d.FromOverall, d.ToOverall)
	}
	first := d.Dimensions[0]
	if first.Dimension != "d1" || first.ToLetter != "A" || first.ToBand != "high" ||
		len(first.Checks) != 3 || first.Checks[0].To != StatusPass ||
		first.Checks[0].From != "" {
		t.Fatalf("first dimension = %+v, want d1 A high with its 3 checks", first)
	}
}

func TestDelta_UnchangedRunIsEmpty(t *testing.T) {
	r, rs := happyRubric()
	prev, cur := Compute(r, rs), Compute(r, rs)
	if d := Delta(&prev, cur); !d.Empty() || len(d.Dimensions) != 0 || d.FirstRun {
		t.Fatalf("unchanged delta = %+v, want empty", d)
	}
}

func TestDelta_ListsOnlyTheDimensionsAndChecksThatMoved(t *testing.T) {
	r, rs := happyRubric()
	prev := Compute(r, rs)
	rs[4].Status = StatusPass // d2.b fail -> pass: 75 C -> 100 A
	cur := Compute(r, rs)
	d := Delta(&prev, cur)
	if d.Empty() || len(d.Dimensions) != 1 {
		t.Fatalf("delta = %+v, want only d2", d)
	}
	got := d.Dimensions[0]
	if got.Dimension != "d2" || got.FromLetter != "C" || got.ToLetter != "A" ||
		len(got.Checks) != 1 || got.Checks[0] != (CheckMove{Check: "d2.b",
		From: StatusFail, To: StatusPass}) {
		t.Fatalf("d2 delta = %+v, want C->A with d2.b fail->pass", got)
	}
}

func TestDelta_CheckMoveWithinOneBandAndLetterIsListed(t *testing.T) {
	r := Rubric{Version: "t/1", Dimensions: []Dimension{dim("cb", spec("cb.a", 95),
		spec("cb.b", 5))}}
	prev := Compute(r, []CheckResult{res("cb.a", StatusPass), res("cb.b", StatusPass)})
	cur := Compute(r, []CheckResult{res("cb.a", StatusPass), res("cb.b", StatusUnknown)})
	d := Delta(&prev, cur)
	if len(d.Dimensions) != 1 || d.Dimensions[0].FromBand != "high" ||
		d.Dimensions[0].ToBand != "high" || len(d.Dimensions[0].Checks) != 1 {
		t.Fatalf("delta = %+v, want the check move listed", d)
	}
	if CoverageBand(59.99) != "low" || CoverageBand(60) != "partial" ||
		CoverageBand(89.99) != "partial" || CoverageBand(90) != "high" {
		t.Fatal("coverage bands are low <60, partial <90, high")
	}
}

func TestDelta_RubricVersionChangeDoesNotCompareLetters(t *testing.T) {
	r, rs := happyRubric()
	prev := Compute(r, rs)
	r.Version = "t/2"
	cur := Compute(r, rs)
	d := Delta(&prev, cur)
	if !d.RubricChanged || d.FirstRun || d.Empty() || len(d.Dimensions) != 5 {
		t.Fatalf("version change = %+v, want rubric changed with the full report", d)
	}
	for _, dd := range d.Dimensions {
		if dd.FromLetter != "" || dd.FromBand != "" {
			t.Fatalf("%s compared across versions: %+v", dd.Dimension, dd)
		}
	}
}

func TestDelta_OverallLetterChangeAloneIsNotEmpty(t *testing.T) {
	prev := Grade{RubricVersion: "t/1", Overall: "B"}
	cur := Grade{RubricVersion: "t/1", Overall: "C"}
	if d := Delta(&prev, cur); d.Empty() || d.FromOverall != "B" || d.ToOverall != "C" {
		t.Fatalf("overall-only delta = %+v, want B -> C", d)
	}
}
