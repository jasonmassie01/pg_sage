package analyzer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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

func cleanPlanPairs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
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
	pool := phase2Pool(t)
	a, _ := recordingAnalyzer(t, pool, 7)
	cleanPlanPairs(t, pool)
	t.Cleanup(func() { cleanPlanPairs(t, pool) })
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
	pool := phase2Pool(t)
	a, rec := recordingAnalyzer(t, pool, 7)
	cleanPlanPairs(t, pool)
	t.Cleanup(func() { cleanPlanPairs(t, pool) })
	ctx := context.Background()
	a.eval = newCycleEval()
	a.checkPlanRegression(ctx)
	stmts := rec.Matching("sage.explain_cache", "plan_json")
	if len(stmts) != 1 {
		t.Fatalf("plan-regression statements = %d, want 1", len(stmts))
	}
	tx, err := pool.Begin(ctx)
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
		FROM generate_series(1, 30) q, generate_series(1, 40) p`,
		planPairQueryBase); err != nil {
		t.Fatalf("seed plans: %v", err)
	}
	if _, err := tx.Exec(ctx, "ANALYZE sage.explain_cache"); err != nil {
		t.Fatal(err)
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
	if carried := plansCarried(plan); carried > 2*queries {
		t.Fatalf("carried %v plans for %v queries, want at most two per query:\n%s",
			carried, queries, plan)
	}
}

// plansCarried is the most plans any node above a scan carries (a scan
// may list every column, decoded or not: PostgreSQL's physical target
// lists).
func plansCarried(plan testdb.PlanNode) float64 {
	var most float64
	plan.Walk(func(n testdb.PlanNode) {
		if strings.Contains(n.NodeType, "Scan") {
			return
		}
		for _, o := range n.Output {
			if strings.Contains(o, "plan_json") {
				most = max(most, n.ActualRows*n.ActualLoops)
				return
			}
		}
	})
	return most
}

// A context key alone cannot authorize provider history on an unbound generic
// rule. The explicit provider-aware optimizer owns resource-scoped reads.
func TestPlanPairsProviderCannotEnterUnboundRegression(t *testing.T) {
	pool := phase2Pool(t)
	cleanPlanPairs(t, pool)
	t.Cleanup(func() { cleanPlanPairs(t, pool) })
	ctx := t.Context()
	qid := int64(planPairQueryBase + 88)
	_, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache(queryid,query_text,plan_json,source,total_cost,execution_time,captured_at,provider_scope_hash,provider_compute_id,analyze_enabled,comparison_key)
 VALUES ($1,'SELECT 1',$2,'lakebase_plan_history',100,1,now()-interval '2 hours','scope-a','compute-a',false,'known-context'),
 ($1,'SELECT 1',$3,'lakebase_plan_history',400,8,now()-interval '1 hour','scope-a','compute-a',false,'known-context')`, qid, planJSON(t, "Index Scan"), planJSON(t, "Seq Scan"))
	if err != nil {
		t.Fatal(err)
	}
	pairs := func() []planPair {
		rows, e := pool.Query(ctx, planPairsSQL)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		var result []planPair
		for rows.Next() {
			var p planPair
			if e = rows.Scan(&p.QueryID, &p.QueryText, &p.CurrentPlan, &p.CurrentCost, &p.CurrentTime, &p.PreviousPlan, &p.PreviousCost, &p.PreviousTime); e != nil {
				t.Fatal(e)
			}
			if p.QueryID == qid {
				result = append(result, p)
			}
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		return result
	}
	if len(pairs()) != 0 {
		t.Fatal("unbound provider plans compared despite absent resource authorization")
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.explain_cache(queryid,query_text,plan_json,source,total_cost,execution_time,captured_at)
 VALUES ($1,'SELECT 1',$2,'auto_explain_log',10,1,now()-interval '4 hours'),($1,'SELECT 1',$3,'auto_explain_log',20,2,now()-interval '3 hours')`, qid, planJSON(t, "Index Scan"), planJSON(t, "Seq Scan"))
	if err != nil {
		t.Fatal(err)
	}
	got := pairs()
	if len(got) != 1 || got[0].CurrentCost != 20 || got[0].PreviousCost != 10 {
		t.Fatalf("provider history displaced compatible local evidence: %+v", got)
	}
}
