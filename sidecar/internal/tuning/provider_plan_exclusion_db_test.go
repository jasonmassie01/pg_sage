package tuning

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPostgresStorePlanExcludesUnboundProviderHistory(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid=$1", qid)
	})
	mustExec(t, pool, `INSERT INTO sage.explain_cache (queryid,captured_at,plan_json,source) VALUES
		($1,now()-interval '1 hour','[{"Plan":{"Node Type":"Index Scan"}}]','auto_explain'),
		($1,now(),'[{"Plan":{"Node Type":"Foreign Scan"}}]','lakebase_plan_history')`, qid)
	store := pgStore(t, pool)
	got, err := store.Plan(ctx, qid, "SELECT 1")
	if err != nil || got.Source != PlanSourceCache || !strings.Contains(string(got.JSON), "Index Scan") {
		t.Fatalf("local cached plan must win: %+v, %v", got, err)
	}
	mustExec(t, pool, "DELETE FROM sage.explain_cache WHERE queryid=$1 AND source='auto_explain'", qid)
	got, err = store.Plan(ctx, qid, "SELECT 1")
	if err != nil || got.Source != PlanSourceExplain || !strings.Contains(string(got.JSON), "Result") {
		t.Fatalf("provider-only history must fall back to fresh EXPLAIN: %+v, %v", got, err)
	}
}
