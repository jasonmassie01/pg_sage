package report

import (
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// The deterministic ranking and the templated text of a review that has
// no model (review-03a).
// No concurrent access tests: Score, Deterministic and Sections are pure
// functions; the templates are parsed once at init and only executed.
// No integration tests: nothing here reads a database.

func pct(v float64) *float64 { return &v }

func cand(id, dim string, size *float64, tier, approval, action string,
	reversible bool) candidate.Candidate {
	return candidate.Candidate{ID: id, Dimension: dim, Kind: "k" + id, Target: "public.t" + id,
		ActionType: action, EvidenceIDs: []string{"index/idx/" + id},
		Prediction: candidate.Prediction{SizePct: size, Tier: tier},
		Approval:   approval, Reversible: reversible}
}

func TestScore_EffectTimesConfidenceOverRisk(t *testing.T) {
	cases := []struct {
		name string
		c    candidate.Candidate
		want float64
	}{
		{"canary automatic reversible", cand("a", "d", pct(-40), "canary",
			candidate.ApprovalAutomatic, "create_index", true), 36},
		{"approval needed halves", cand("b", "d", pct(40), "canary",
			candidate.ApprovalRequired, "create_index", true), 18},
		{"irreversible executed step thirds", cand("c", "d", pct(30), "fleet",
			candidate.ApprovalRequired, "drop_index", false), 10},
		{"advice is not executed", cand("d", "d", pct(30), "fleet",
			candidate.ApprovalRequired, candidate.ActionAdvice, false), 15},
		{"unknown size counts 1", cand("e", "d", nil, "plan_only",
			candidate.ApprovalAutomatic, "create_index", true), 0.5},
		{"unknown tier counts as none", cand("f", "d", pct(10), "lab",
			candidate.ApprovalAutomatic, "create_index", true), 2.5},
		{"zero size counts 1", cand("g", "d", pct(0), candidate.TierNone,
			candidate.ApprovalAutomatic, "create_index", true), 0.25},
		{"NaN size counts 1", cand("h", "d", pct(math.NaN()), "clone",
			candidate.ApprovalAutomatic, "create_index", true), 0.75},
		{"empty approval fails closed", cand("i", "d", pct(10), "clone", "",
			"create_index", true), 3.75},
	}
	for _, tc := range cases {
		if got := Score(tc.c); got != tc.want {
			t.Errorf("%s: score %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeterministic_OrdersByScoreThenID(t *testing.T) {
	cands := []candidate.Candidate{
		cand("b", "d", pct(10), "clone", candidate.ApprovalAutomatic, "create_index", true),
		cand("a", "d", pct(10), "clone", candidate.ApprovalAutomatic, "create_index", true),
		cand("c", "d", pct(90), "fleet", candidate.ApprovalAutomatic, "create_index", true),
	}
	got := Deterministic(cands)
	if len(got) != 3 || got[0].CandidateID != "c" || got[1].CandidateID != "a" ||
		got[2].CandidateID != "b" {
		t.Fatalf("order = %+v, want c, a, b", got)
	}
	for i, r := range got {
		if r.Rank != i+1 || r.Decision != DecisionKeep || r.Origin != OriginDeterministic ||
			len(r.EvidenceIDs) != 1 || r.Target == "" {
			t.Errorf("entry %d = %+v", i, r)
		}
	}
	got[0].EvidenceIDs[0] = "changed"
	if cands[2].EvidenceIDs[0] != "index/idx/c" {
		t.Fatal("the ranking shares the candidate's evidence slice")
	}
	if len(Deterministic(nil)) != 0 {
		t.Fatal("no candidates gave a ranking")
	}
}

func grade() *rubric.Grade {
	return &rubric.Grade{RubricVersion: "v1", Overall: "B", Dimensions: []rubric.DimensionGrade{
		{Dimension: "indexes", Title: "Indexes", Letter: "C", Score: 61.5, Coverage: 80},
		{Dimension: "vacuum", Title: "Vacuum", Letter: "A", Score: 100, Coverage: 100}}}
}

func TestSections_GradeTextPerDimensionWithItsCandidates(t *testing.T) {
	ranked := Deterministic([]candidate.Candidate{
		cand("a", "indexes", pct(50), "clone", candidate.ApprovalAutomatic, "x", true),
		cand("b", "indexes", pct(5), "clone", candidate.ApprovalAutomatic, "x", true),
		cand("z", "schema", pct(5), "clone", candidate.ApprovalAutomatic, "x", true),
	})
	secs, err := Sections(grade(), ranked)
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	if len(secs) != 3 {
		t.Fatalf("sections = %+v, want indexes, vacuum and the ungraded schema", secs)
	}
	want := []string{"Indexes: grade C, 61.5% of the applicable checks passed with 80% of " +
		"them run. 2 candidates ranked here; first: ka on public.ta.",
		"Vacuum: grade A, 100% of the applicable checks passed with 100% of them run. " +
			"No candidate for this dimension.",
		"schema: not graded by this review's rubric. 1 candidate ranked here; first: kz on " +
			"public.tz."}
	for i, w := range want {
		if secs[i].Text != w || secs[i].Origin != OriginDeterministic {
			t.Errorf("section %d = %q (%s), want %q", i, secs[i].Text, secs[i].Origin, w)
		}
	}
	if secs[0].Letter != "C" || secs[1].Dimension != "vacuum" {
		t.Errorf("section fields = %+v", secs[:2])
	}
}

func TestSections_NoGradeAndNoCandidates(t *testing.T) {
	secs, err := Sections(nil, nil)
	if err != nil || len(secs) != 1 || secs[0].Text != "This review has no grade yet." {
		t.Fatalf("no grade, no candidates: %+v %v", secs, err)
	}
	ranked := Deterministic([]candidate.Candidate{cand("a", "indexes", nil, "none",
		candidate.ApprovalRequired, "x", true)})
	secs, err = Sections(nil, ranked)
	if err != nil || len(secs) != 2 ||
		!strings.Contains(secs[0].Text, "1 candidates were ranked without one") {
		t.Fatalf("no grade with a candidate: %+v %v", secs, err)
	}
	secs, err = Sections(grade(), nil)
	if err != nil || len(secs) != 2 || !strings.Contains(secs[0].Text, "No candidate") {
		t.Fatalf("grade without candidates: %+v %v", secs, err)
	}
}

// A target is data: template syntax inside it is printed, not executed.
func TestSections_TargetTextIsNotExecuted(t *testing.T) {
	c := cand("a", "indexes", nil, "none", candidate.ApprovalRequired, "x", true)
	c.Target = `{{template "nograde" 1}}`
	secs, err := Sections(grade(), Deterministic([]candidate.Candidate{c}))
	if err != nil || !strings.Contains(secs[0].Text, `on {{template "nograde" 1}}.`) {
		t.Fatalf("section = %+v %v, want the target printed as text", secs, err)
	}
}
