package tuning

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
)

// Boundaries the mutation run found unpinned: the class status and the
// pooled fallback at exactly the minimum, the single-statement rule on
// its own, and the facts gate when the classification did not see the
// fixture fact.

func TestCalibrate_ClassStatusAtTheMinimum(t *testing.T) {
	at := Calibrate(improvedOutcomes("guc", "model", -40, 5, 1), 5)
	if at.Classes[0].Status != StatusCalibrated {
		t.Fatalf("5 outcomes with a minimum of 5: %+v", at.Classes[0])
	}
	below := Calibrate(improvedOutcomes("guc", "model", -40, 4, 1), 5)
	if below.Classes[0].Status != StatusUncalibrated {
		t.Fatalf("4 outcomes with a minimum of 5: %+v", below.Classes[0])
	}
}

func TestCalibrate_PooledFallbackAtTheMinimum(t *testing.T) {
	samples := append(improvedOutcomes("index_create", "hypopg", -30, 3, 3),
		improvedOutcomes("index_create", "hypopg", -5, 2, 1)...)
	c := Calibrate(samples, 5).ConfidenceFor("index_create", "hypopg", -80)
	if c.Status != StatusCalibrated || c.Basis != BasisClassMethod || c.N != 5 ||
		c.Hits != 4 {
		t.Fatalf("no bin has 5 but the pool has exactly 5: %+v", c)
	}
}

func TestSingleStatement(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE INDEX a ON t (x)":               true,
		"CREATE INDEX a ON t (x);":              true,
		"SELECT 1 WHERE note = ';'":             true,
		"SELECT 1 /* ; */":                      true,
		"CREATE INDEX a ON t (x); DROP TABLE t": false,
		"SELECT 1;SELECT 2":                     false,
		"  ;  ":                                 false,
		"":                                      false,
		"SELECT 1 -- trailing; comment\n; DROP t":  false,
		`SELECT "semi;colon" FROM t`:               true,
		"CREATE INDEX a ON t (x);; ":               false,
		"SELECT $$;$$":                             false,
		"SELECT 1 WHERE a = 'it''s; fine'":         true,
		"ANALYZE t; VACUUM t":                      false,
		"CREATE INDEX a ON t (x)\n;\n":             true,
		"SELECT 1; -- one statement and a comment": false,
	} {
		if _, got := singleStatement(sql); got != want {
			t.Errorf("singleStatement(%q) = %t, want %t", sql, got, want)
		}
	}
}

func TestJudge_FixtureFactBindsEvenWhenTheClassificationMissedIt(t *testing.T) {
	h := newHarness(t)
	cur := validationSnap()
	fixture := confirmedFact(14, facts.TypeTestFixture, facts.KindSchema, "public", nil)
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0),
		[]facts.Fact{fixture}, nil)
	j := v.judge(context.Background(), ordersCase(), ordersEvidence(),
		dropProposal("public.orders_customer_idx"))
	if j.Verdict != VerdictRejected || j.Reason != ReasonNotWorkload || j.Finding != nil {
		t.Fatalf("the facts gate drops a fixture's proposal: %+v", j)
	}
}
