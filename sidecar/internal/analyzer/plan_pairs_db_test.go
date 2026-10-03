package analyzer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The plan-regression rule compares the newest two plans of each query
// (static audit A_snapshots P5): it numbered every plan of the last seven
// days and so detoasted every plan_json there, although it keeps two per
// query. It must read at most two plans per query. It also failed on
// plans captured without an execution time or query text (nullable
// columns): "cannot scan NULL into *float64" ended every pass (seen in
// every performance gate run).

const planPairQueryBase = 917_000_000

func cleanPlanPairs(t *testing.T) {
	t.Helper()
	pool := phase2Pool(t)
	if _, err := pool.Exec(context.Background(), `DELETE FROM sage.explain_cache
		WHERE queryid BETWEEN $1 AND $1 + 999`, planPairQueryBase); err != nil {
		t.Fatalf("clean plans: %v", err)
	}
}

func planJSON(t *testing.T, node string) string {
	t.Helper()
	raw, err := json.Marshal([]map[string]any{{"Plan": map[string]any{
		"Node Type": node, "Relation Name": "orders", "Plan Rows": 100}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPlanRegression_NullTimingAndTextDoNotFailThePass(t *testing.T) {
	a, _ := recordingAnalyzer(t, 7)
	cleanPlanPairs(t)
	t.Cleanup(func() { cleanPlanPairs(t) })
	pool := phase2Pool(t)
	ctx := context.Background()
	qid := int64(planPairQueryBase + 1)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at, queryid,
		query_text, plan_json, source, total_cost, execution_time) VALUES
		(now() - interval '2 hours', $1, NULL, $2::jsonb, 'collector', 100, NULL),
		(now() - interval '1 hour', $1, NULL, $3::jsonb, 'collector', 400, NULL)`,
		qid, planJSON(t, "Index Scan"), planJSON(t, "Seq Scan")); err != nil {
		t.Fatalf("seed plans: %v", err)
	}
	a.eval = newCycleEval()
	findings := a.checkPlanRegression(ctx)
	if a.eval.failed["plan_regression"] {
		t.Fatal("a plan without execution time or query text failed the rule")
	}
	found := false
	for _, f := range findings {
		found = found || strings.Contains(f.ObjectIdentifier, "917000001")
	}
	if !found {
		t.Fatalf("the regressed query (cost 100 -> 400, index -> seq) has no finding: %+v",
			findings)
	}
}

func TestPlanRegression_ReadsTwoPlansPerQuery(t *testing.T) {
	a, rec := recordingAnalyzer(t, 7)
	cleanPlanPairs(t)
	t.Cleanup(func() { cleanPlanPairs(t) })
	ctx := context.Background()
	a.eval = newCycleEval()
	a.checkPlanRegression(ctx)
	stmts := rec.Matching("sage.explain_cache", "plan_json")
	if len(stmts) != 1 {
		t.Fatalf("plan-regression statements = %d, want 1", len(stmts))
	}
	tx, err := phase2Pool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at, queryid,
		query_text, plan_json, source, total_cost, execution_time)
		SELECT now() - (p * interval '1 hour'), $1 + q, 'SELECT ' || q,
		       jsonb_build_array(jsonb_build_object('Plan', jsonb_build_object(
		         'Node Type', 'Seq Scan', 'Relation Name', 'orders',
		         'Pad', repeat(md5(q::text), 40)))), 'collector', 100 + p, 1
		FROM generate_series(1, 30) q, generate_series(1, 40) p;
		ANALYZE sage.explain_cache`, planPairQueryBase); err != nil {
		t.Fatalf("seed plans: %v", err)
	}
	var queries float64
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT queryid) FROM sage.explain_cache
		WHERE captured_at > now() - interval '7 days'`).Scan(&queries); err != nil {
		t.Fatal(err)
	}
	plan, err := testdb.Explain(ctx, tx, "ANALYZE, VERBOSE", stmts[0].SQL, stmts[0].Args...)
	if err != nil {
		t.Fatal(err)
	}
	var plansRead float64
	plan.Walk(func(n testdb.PlanNode) {
		if n.Relation != "explain_cache" {
			return
		}
		for _, o := range n.Output {
			if strings.Contains(o, "plan_json") {
				plansRead += n.ActualRows * n.ActualLoops
				return
			}
		}
	})
	if plansRead > 2*queries {
		t.Fatalf("read %v plans for %v queries, want at most two per query:\n%s",
			plansRead, queries, plan)
	}
}
