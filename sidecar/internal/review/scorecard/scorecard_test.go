package scorecard

import (
	"strings"
	"testing"
	"time"
)

// Build is pure arithmetic over the rows the store read. No concurrent
// access tests here: Build shares nothing between calls.

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func run(id int64, models ...string) Run {
	return Run{ID: id, Database: "shop", Status: StatusDone,
		FinishedAt: t0.Add(time.Duration(id) * time.Hour), Models: models}
}

func step(runID int64, origin, ref, verdict string, accepted, reverted bool) Step {
	return Step{RunID: runID, Origin: origin, OriginRef: ref, ActionType: "create_index",
		SQL: "CREATE INDEX CONCURRENTLY i" + ref + verdict + " ON t (a)", Accepted: accepted,
		Verdict: verdict, Reverted: reverted}
}

func TestBuild_CountsEveryOutcomeByOriginAndGenerator(t *testing.T) {
	runs := []Run{run(1, "m1"), run(2, "m1")}
	steps := []Step{
		step(1, OriginGenerator, "indexes", VerdictImproved, true, false),
		step(1, OriginGenerator, "schemadebt", VerdictNoChange, true, false),
		step(1, OriginModel, "m1", VerdictRegressed, true, true),
		step(1, OriginModel, "m1", VerdictInconclusive, true, false),
		step(2, OriginModel, "m1", VerdictExpired, true, false),
		step(2, OriginModel, "m1", VerdictUnverifiable, true, false),
		step(2, OriginModel, "m1", "", false, false),
		step(2, OriginPerson, "alice", VerdictImproved, true, false),
	}
	sc, err := Build("shop", t0, runs, steps)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if sc.Reviews != 2 || len(sc.PerReview) != 2 || sc.Database != "shop" {
		t.Fatalf("scorecard = %+v, want 2 reviews of shop", sc)
	}
	m := sc.ByOrigin[OriginModel]
	want := Counts{Proposed: 5, Accepted: 4, Regressed: 1, Inconclusive: 2, Unverifiable: 1,
		Pending: 1, Reverted: 1, Decided: 1}
	if m != want {
		t.Fatalf("model counts = %+v, want %+v", m, want)
	}
	g := sc.ByOrigin[OriginGenerator]
	if g.Proposed != 2 || g.Improved != 1 || g.NoChange != 1 || g.Decided != 2 ||
		g.Accepted != 2 {
		t.Fatalf("generator counts = %+v", g)
	}
	if p := sc.ByOrigin[OriginPerson]; p.Improved != 1 || p.Decided != 1 {
		t.Fatalf("person counts = %+v", p)
	}
	if sc.ByGenerator["indexes"].Improved != 1 || sc.ByGenerator["schemadebt"].NoChange != 1 ||
		len(sc.ByGenerator) != 2 {
		t.Fatalf("by generator = %+v, want indexes and schemadebt only", sc.ByGenerator)
	}
	if r := sc.PerReview[0]; r.Run.ID != 2 || r.ByOrigin[OriginModel].Proposed != 3 {
		t.Fatalf("newest review first: %+v", r)
	}
	if sc.ModelOnlyAccepted != 4 || sc.PerReview[1].ModelOnlyAccepted != 2 {
		t.Fatalf("model-only accepted = %d (run 1: %d), want 4 and 2", sc.ModelOnlyAccepted,
			sc.PerReview[1].ModelOnlyAccepted)
	}
}

// A model step a generator of the same review also proposed (same action
// and SQL, spacing and case aside) is not the model's alone.
func TestBuild_ModelStepAlsoProposedByAGeneratorIsNotModelOnly(t *testing.T) {
	gen := Step{RunID: 1, Origin: OriginGenerator, OriginRef: "indexes",
		ActionType: "create_index", SQL: "CREATE INDEX CONCURRENTLY i ON t (a)"}
	dup := Step{RunID: 1, Origin: OriginModel, OriginRef: "m1", ActionType: "create_index",
		SQL: "create index  concurrently i\n ON t (a)", Accepted: true}
	other := Step{RunID: 2, Origin: OriginModel, OriginRef: "m1", ActionType: "create_index",
		SQL: "CREATE INDEX CONCURRENTLY i ON t (a)", Accepted: true}
	sc, err := Build("shop", t0, []Run{run(1, "m1"), run(2, "m1")}, []Step{gen, dup, other})
	if err != nil {
		t.Fatal(err)
	}
	if sc.ModelOnlyAccepted != 1 || sc.ByOrigin[OriginModel].Accepted != 2 {
		t.Fatalf("model-only accepted = %d of %d, want 1 of 2 (run 2's step only)",
			sc.ModelOnlyAccepted, sc.ByOrigin[OriginModel].Accepted)
	}
}

func TestBuild_EmptyIsZeroReviewsNotAnError(t *testing.T) {
	sc, err := Build("shop", t0, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if sc.Reviews != 0 || len(sc.PerReview) != 0 || sc.ByOrigin == nil ||
		sc.ByGenerator == nil || sc.ByOrigin[OriginModel] != (Counts{}) {
		t.Fatalf("empty scorecard = %+v, want zero reviews and empty maps", sc)
	}
	// A review that proposed nothing still counts as a review.
	sc, err = Build("shop", t0, []Run{run(1, "m1")}, nil)
	if err != nil || sc.Reviews != 1 || sc.PerReview[0].ByOrigin == nil {
		t.Fatalf("plan-less review: %+v %v", sc, err)
	}
}

func TestBuild_RefusesRowsItCannotCount(t *testing.T) {
	for name, s := range map[string]Step{
		"unknown run":     step(9, OriginModel, "m1", VerdictImproved, true, false),
		"unknown origin":  step(1, "agent", "x", VerdictImproved, true, false),
		"unknown verdict": step(1, OriginModel, "m1", "great", true, false),
		"empty ref":       step(1, OriginGenerator, "", VerdictImproved, true, false),
	} {
		if name == "empty ref" {
			s.OriginRef = ""
		}
		_, err := Build("shop", t0, []Run{run(1, "m1")}, []Step{s})
		if err == nil || !strings.Contains(err.Error(), "scorecard") {
			t.Errorf("%s: error %v, want a scorecard error", name, err)
		}
	}
	if _, err := Build("", t0, nil, nil); err == nil {
		t.Error("a scorecard of no database was built")
	}
}

// Decided is improved, no change or regressed, or any revert: a reverted
// step that the verifier called inconclusive still decided its fate.
func TestCounts_DecidedIncludesRevertsOnce(t *testing.T) {
	var c Counts
	for _, s := range []Step{
		step(1, OriginModel, "m1", VerdictInconclusive, true, true),
		step(1, OriginModel, "m1", VerdictRegressed, true, true),
		step(1, OriginModel, "m1", VerdictImproved, true, false),
	} {
		if err := c.add(s); err != nil {
			t.Fatal(err)
		}
	}
	if c.Decided != 3 || c.Reverted != 2 || c.Improved != 1 || c.Inconclusive != 1 {
		t.Fatalf("counts = %+v, want 3 decided, 2 reverted", c)
	}
}

func TestNormalizeSQL_CollapsesSpacingAndCase(t *testing.T) {
	if got := normalizeSQL("  CREATE\tINDEX  i\n ON t (a);  "); got !=
		"create index i on t (a)" {
		t.Fatalf("normalizeSQL = %q", got)
	}
}
