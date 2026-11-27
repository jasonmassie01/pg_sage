package planfresh

import (
	"context"
	"strings"
	"testing"
)

// rca-04: the on-demand EXPLAIN is an estimate (explain_estimate); a plan
// from the auto_explain log ran (auto_explain_log). The reader names the
// row it used, and of the fresh captures it prefers the executed one, so
// an estimate taken after a logged plan never hides the plan's actual rows.
// The optimizer no longer captures plans (indexes-05); this reader is the
// one that chooses between cached plans (tuner and tuning agent).

// executedPlanOn is an auto_explain log plan of s.t with actual rows.
func executedPlanOn(s string) string {
	return `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Schema":"` + s +
		`","Actual Rows":200,"Actual Loops":1}}]`
}

func TestReader_OnlyAnEstimateIsReportedAsTheEstimate(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", planOn(s), "explain_estimate")
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceCache || p.Capture != "explain_estimate" ||
		p.HasActuals {
		t.Fatalf("estimate-only cache = %+v, %v; want capture explain_estimate, no actuals",
			p, err)
	}
	if got := p.Provenance(p.CapturedAt)["capture"]; got != "explain_estimate" {
		t.Fatalf("provenance capture = %v, want explain_estimate", got)
	}
}

func TestReader_ExecutedPlanWinsOverANewerEstimate(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", executedPlanOn(s), "auto_explain_log")
	cache(t, pool, qid, "-1 second", planOn(s), "explain_estimate") // newer
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Capture != "auto_explain_log" || !p.HasActuals ||
		!strings.Contains(string(p.JSON), "Actual Rows") {
		t.Fatalf("with both cached = %+v, %v; want the executed auto_explain_log plan", p, err)
	}
}

// Boundary: a stale executed plan never beats a fresh estimate.
func TestReader_StaleExecutedPlanLosesToAFreshEstimate(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "30 hours", executedPlanOn(s), "auto_explain_log")
	cache(t, pool, qid, "0 seconds", planOn(s), "explain_estimate")
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Capture != "explain_estimate" || p.HasActuals {
		t.Fatalf("stale log + fresh estimate = %+v, %v; want the fresh estimate", p, err)
	}
}

// Of two fresh executed plans the newest is used, as before.
func TestReader_NewestOfTwoExecutedPlansWins(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "0 seconds", executedPlanOn(s), "auto_explain_log")
	newer := strings.Replace(executedPlanOn(s), `"Actual Rows":200`, `"Actual Rows":7`, 1)
	cache(t, pool, qid, "-1 second", newer, "auto_explain_log")
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || !strings.Contains(string(p.JSON), `"Actual Rows": 7`) {
		t.Fatalf("two executed plans = %+v, %v; want the newest (7 rows)", p, err)
	}
}

// auto_explain logs actual rows only with log_analyze on, which First Look
// does not ask for (its overhead hits every statement). A logged plan
// without actuals is still the plan that ran, with the real parameters, so
// it beats a newer on-demand estimate too; it reports no actuals.
func TestReader_LoggedPlanWithoutActualsWinsOverANewerEstimate(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	logged := strings.Replace(planOn(s), `"Seq Scan"`, `"Index Scan"`, 1)
	if logged == planOn(s) {
		t.Fatal("fixture plan has no Seq Scan node to tell the two plans apart")
	}
	cache(t, pool, qid, "0 seconds", logged, "auto_explain_log")
	cache(t, pool, qid, "-1 second", planOn(s), "explain_estimate") // newer
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Capture != "auto_explain_log" || p.HasActuals ||
		!strings.Contains(string(p.JSON), "Index Scan") {
		t.Fatalf("logged plan without actuals + newer estimate = %+v, %v; "+
			"want the auto_explain_log plan, no actuals", p, err)
	}
}
