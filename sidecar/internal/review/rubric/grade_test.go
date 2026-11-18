package rubric

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// review-01a: the grade arithmetic. No concurrent access tests: Compute
// is a pure function of its arguments.

func spec(id string, w int) CheckSpec {
	return CheckSpec{ID: id, Source: SourceFinding, Ref: "temp_spill", Weight: w}
}

func dim(id string, checks ...CheckSpec) Dimension {
	return Dimension{ID: id, Title: strings.ToUpper(id), Checks: checks}
}

func res(id string, s Status) CheckResult {
	return CheckResult{Check: id, Status: s, Evidence: []string{"finding:1"}}
}

func dimOf(t *testing.T, g Grade, id string) DimensionGrade {
	t.Helper()
	for _, d := range g.Dimensions {
		if d.Dimension == id {
			return d
		}
	}
	t.Fatalf("no dimension %s in %+v", id, g)
	return DimensionGrade{}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.005 }

// The table of the happy path, by hand:
//
//	d1: 2 pass, 2 pass, 1 warn      4.5/5 = 90.00 A, coverage 100
//	d2: 3 pass, 1 fail              3/4   = 75.00 C, coverage 100
//	d3: 2 pass, 2 unknown, 1 pass   3/5   = 60.00 D, coverage 60 (graded)
//	d4: 1 pass, 1 not_applicable    1/1   = 100   A, coverage 100
//	d5: 1 unknown, 1 pass           1/2   = 50.00 unknown, coverage 50
//
// Overall: the mean of the five scores is 75 (C), capped one letter above
// the worst dimension's letter of score (d5, F): D. Coverage 14/17.
func happyRubric() (Rubric, []CheckResult) {
	r := Rubric{Version: "t/1", Dimensions: []Dimension{
		dim("d1", spec("d1.a", 2), spec("d1.b", 2), spec("d1.c", 1)),
		dim("d2", spec("d2.a", 3), spec("d2.b", 1)),
		dim("d3", spec("d3.a", 2), spec("d3.b", 2), spec("d3.c", 1)),
		dim("d4", spec("d4.a", 1), spec("d4.b", 1)),
		dim("d5", spec("d5.a", 1), spec("d5.b", 1)),
	}}
	results := []CheckResult{res("d1.a", StatusPass), res("d1.b", StatusPass),
		res("d1.c", StatusWarn), res("d2.a", StatusPass), res("d2.b", StatusFail),
		res("d3.a", StatusPass), res("d3.b", StatusUnknown), res("d3.c", StatusPass),
		res("d4.a", StatusPass), res("d4.b", StatusNotApplicable),
		res("d5.a", StatusUnknown), res("d5.b", StatusPass)}
	return r, results
}

func TestCompute_HappyPathTable(t *testing.T) {
	r, results := happyRubric()
	g := Compute(r, results)
	want := []struct {
		id              string
		letter          Letter
		score, coverage float64
	}{{"d1", "A", 90, 100}, {"d2", "C", 75, 100}, {"d3", "D", 60, 60},
		{"d4", "A", 100, 100}, {"d5", "", 50, 50}}
	for _, w := range want {
		d := dimOf(t, g, w.id)
		if d.Letter != w.letter || !near(d.Score, w.score) || !near(d.Coverage, w.coverage) {
			t.Errorf("%s = %q %.2f cov %.2f, want %q %.2f cov %.2f", w.id, d.Letter, d.Score,
				d.Coverage, w.letter, w.score, w.coverage)
		}
	}
	if g.Overall != "D" || !near(g.OverallScore, 75) || g.Known != 4 {
		t.Fatalf("overall = %q %.2f known %d, want D 75.00 known 4", g.Overall,
			g.OverallScore, g.Known)
	}
	if !near(g.CoveragePct, 82.35) || g.RubricVersion != "t/1" {
		t.Fatalf("coverage %.2f version %q, want 82.35 t/1", g.CoveragePct, g.RubricVersion)
	}
	if got := dimOf(t, g, "d1").Checks[2]; got.Check != "d1.c" || got.Weight != 1 ||
		got.Status != StatusWarn || len(got.Evidence) != 1 {
		t.Fatalf("d1.c result = %+v, want the warn with its weight and evidence", got)
	}
}

func TestCompute_NoResultsIsEightUnknownsNeverA(t *testing.T) {
	for _, results := range [][]CheckResult{nil, {}} {
		g := Compute(V1(), results)
		if len(g.Dimensions) != 8 || g.Overall != "" || g.OverallScore != 0 ||
			g.CoveragePct != 0 || g.Known != 0 {
			t.Fatalf("empty grade = %+v, want 8 dimensions and an unknown overall", g)
		}
		for _, d := range g.Dimensions {
			if d.Letter != "" || d.Score != 0 || d.Coverage != 0 {
				t.Errorf("%s = %q %.2f %.2f, want unknown", d.Dimension, d.Letter, d.Score,
					d.Coverage)
			}
			for _, c := range d.Checks {
				if c.Status != StatusUnknown || !strings.Contains(c.Reason, "no result") {
					t.Errorf("%s = %s %q, want unknown with a reason", c.Check, c.Status,
						c.Reason)
				}
			}
		}
	}
}

func TestCompute_IgnoresResultsForChecksTheRubricLacks(t *testing.T) {
	r := Rubric{Version: "t/1", Dimensions: []Dimension{dim("d1", spec("d1.a", 1))}}
	g := Compute(r, []CheckResult{res("d1.a", StatusFail), res("ghost", StatusPass)})
	if d := dimOf(t, g, "d1"); d.Score != 0 || len(d.Checks) != 1 || d.Letter != "F" {
		t.Fatalf("d1 = %+v, want only d1.a, failed", d)
	}
}

func TestLetterFor_Boundaries(t *testing.T) {
	cases := map[float64]Letter{100: "A", 90: "A", 89.99: "B", 80: "B", 79.99: "C",
		70: "C", 69.99: "D", 60: "D", 59.99: "F", 0: "F"}
	for score, want := range cases {
		if got := LetterFor(score); got != want {
			t.Errorf("LetterFor(%.2f) = %q, want %q", score, got, want)
		}
	}
}

func weighted(id string, pass, other int, otherStatus Status) (Rubric, []CheckResult) {
	r := Rubric{Version: "t/1", Dimensions: []Dimension{
		dim(id, spec(id+".p", pass), spec(id+".o", other))}}
	return r, []CheckResult{res(id+".p", StatusPass), res(id+".o", otherStatus)}
}

func TestCompute_ScoreBoundary(t *testing.T) {
	r, rs := weighted("b", 8999, 1001, StatusFail)
	if d := Compute(r, rs).Dimensions[0]; !near(d.Score, 89.99) || d.Letter != "B" {
		t.Fatalf("89.99 = %.2f %q, want B", d.Score, d.Letter)
	}
	r, rs = weighted("a", 9000, 1000, StatusFail)
	if d := Compute(r, rs).Dimensions[0]; !near(d.Score, 90) || d.Letter != "A" {
		t.Fatalf("90.00 = %.2f %q, want A", d.Score, d.Letter)
	}
}

func TestCompute_CoverageBoundary(t *testing.T) {
	r, rs := weighted("u", 599, 401, StatusUnknown)
	if d := Compute(r, rs).Dimensions[0]; !near(d.Coverage, 59.9) || d.Letter != "" ||
		!near(d.Score, 59.9) {
		t.Fatalf("coverage 59.9 = %+v, want unknown with the score kept", d)
	}
	r, rs = weighted("g", 600, 400, StatusUnknown)
	if d := Compute(r, rs).Dimensions[0]; !near(d.Coverage, 60) || d.Letter != "D" {
		t.Fatalf("coverage 60.0 = %+v, want graded D", d)
	}
}

func TestCompute_CriticalFailOrUnknownCapsAtD(t *testing.T) {
	for _, s := range []Status{StatusFail, StatusUnknown} {
		r := Rubric{Version: "t/1", Dimensions: []Dimension{dim("c", spec("c.big", 100000),
			CheckSpec{ID: "c.crit", Source: SourceFinding, Ref: "temp_spill", Weight: 1,
				Critical: true})}}
		d := Compute(r, []CheckResult{res("c.big", StatusPass), res("c.crit", s)}).Dimensions[0]
		if !near(d.Score, 100) || d.Letter != "D" {
			t.Errorf("critical %s on a 100 score = %.2f %q, want D", s, d.Score, d.Letter)
		}
	}
	r := Rubric{Version: "t/1", Dimensions: []Dimension{dim("c", spec("c.a", 9),
		CheckSpec{ID: "c.crit", Source: SourceFinding, Ref: "temp_spill", Weight: 1,
			Critical: true})}}
	d := Compute(r, []CheckResult{res("c.a", StatusPass), res("c.crit", StatusWarn)})
	if d.Dimensions[0].Letter != "A" {
		t.Fatalf("a critical warn = %q, want no cap (A at 95)", d.Dimensions[0].Letter)
	}
}

func knownDims(n int) (Rubric, []CheckResult) {
	r := Rubric{Version: "t/1"}
	var rs []CheckResult
	for i := range 8 {
		id := fmt.Sprintf("k%d", i)
		r.Dimensions = append(r.Dimensions, dim(id, spec(id+".a", 1)))
		st := StatusUnknown
		if i < n {
			st = StatusPass
		}
		rs = append(rs, res(id+".a", st))
	}
	return r, rs
}

func TestCompute_OverallNeedsFourKnownDimensions(t *testing.T) {
	r, rs := knownDims(3)
	if g := Compute(r, rs); g.Overall != "" || g.Known != 3 || !near(g.OverallScore, 37.5) {
		t.Fatalf("three known = %q known %d score %.2f, want unknown, 37.50 stored",
			g.Overall, g.Known, g.OverallScore)
	}
	r, rs = knownDims(4)
	if g := Compute(r, rs); g.Overall != "F" || g.Known != 4 || !near(g.OverallScore, 50) {
		t.Fatalf("four known = %q known %d score %.2f, want graded F at 50", g.Overall,
			g.Known, g.OverallScore)
	}
}

func TestCompute_OverallIsAtMostOneLetterAboveTheWorst(t *testing.T) {
	r := Rubric{Version: "t/1"}
	var rs []CheckResult
	for i := range 5 {
		id := fmt.Sprintf("o%d", i)
		r.Dimensions = append(r.Dimensions, dim(id, spec(id+".a", 9), spec(id+".b", 1)))
		rs = append(rs, res(id+".a", StatusPass), res(id+".b", StatusPass))
	}
	rs[1].Status = StatusFail // o0.b: o0 is 9/10 = 90.00, A
	rs[3].Status = StatusFail // o1.b, weight 3: o1 is 9/12 = 75.00, C
	r.Dimensions[1].Checks[1].Weight = 3
	g := Compute(r, rs) // o1: 9/12 = 75.00 C; mean (90+75+300)/5 = 93 A, capped at B
	if dimOf(t, g, "o1").Letter != "C" || g.Overall != "B" || !near(g.OverallScore, 93) {
		t.Fatalf("overall = %q %.2f (o1 %q), want B at 93.00 under a C", g.Overall,
			g.OverallScore, dimOf(t, g, "o1").Letter)
	}
}

func TestCompute_AllNotApplicableDimensionIsLeftOut(t *testing.T) {
	r, rs := knownDims(8)
	r.Dimensions = append(r.Dimensions, dim("na", spec("na.a", 5)))
	rs = append(rs, res("na.a", StatusNotApplicable))
	g := Compute(r, rs)
	if d := dimOf(t, g, "na"); d.Letter != "" || d.Coverage != 0 {
		t.Fatalf("all n/a = %+v, want unknown", d)
	}
	if g.Overall != "A" || !near(g.OverallScore, 100) || !near(g.CoveragePct, 100) {
		t.Fatalf("overall = %q %.2f cov %.2f, want A 100 (n/a left out)", g.Overall,
			g.OverallScore, g.CoveragePct)
	}
}

func TestLetter_UnknownMarshalsAsNull(t *testing.T) {
	b, err := json.Marshal(struct {
		A, B Letter
	}{"", "B"})
	if err != nil || string(b) != `{"A":null,"B":"B"}` {
		t.Fatalf("marshal = %s %v, want null for unknown", b, err)
	}
	var back struct{ A, B Letter }
	if err := json.Unmarshal(b, &back); err != nil || back.A != "" || back.B != "B" {
		t.Fatalf("unmarshal = %+v %v", back, err)
	}
}
