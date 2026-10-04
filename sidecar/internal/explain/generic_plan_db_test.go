package explain

import (
	"context"
	"strings"
	"testing"
)

// A pg_stat_statements text has $n placeholders and no values. Planning
// it with NULLs folds "col = NULL" to a constant-false Result (cost 0),
// which says nothing about the real query: unbound parameters must get
// the generic plan instead.
func TestExplainUnboundParametersGetGenericPlan(t *testing.T) {
	pool := liveExplainPool(t)
	ctx := t.Context()
	for _, sql := range []string{
		"DROP TABLE IF EXISTS explain_generic_probe",
		"CREATE TABLE explain_generic_probe (id int, v int)",
		"INSERT INTO explain_generic_probe SELECT i, i % 100 FROM generate_series(1, 5000) i",
		"ANALYZE explain_generic_probe",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP TABLE IF EXISTS explain_generic_probe")
		if err != nil {
			t.Errorf("drop probe: %v", err)
		}
	})
	ex := New(pool, liveConfig(10000), func(string, string, ...any) {})
	res, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT id FROM explain_generic_probe WHERE v = $1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.EstimatedCost <= 0 {
		t.Fatalf("estimated cost %.2f: the plan was folded on NULL parameters", res.EstimatedCost)
	}
	if !strings.Contains(string(res.PlanJSON), "explain_generic_probe") {
		t.Fatalf("plan does not scan the table: %s", res.PlanJSON)
	}
	bound, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT id FROM explain_generic_probe WHERE v = $1", Params: []string{"7"}})
	if err != nil {
		t.Fatalf("Explain with a value: %v", err)
	}
	if !strings.Contains(string(bound.PlanJSON), "explain_generic_probe") {
		t.Fatalf("bound plan does not scan the table: %s", bound.PlanJSON)
	}
}
