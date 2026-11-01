package verify

import (
	"context"
	"testing"
	"time"
)

func TestEstimateErrorsExcludesUnboundProviderHistory(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := context.Background()
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid=$1", qid)
	})
	// Twenty newer asynchronous plans would consume the entire per-target
	// limit and fabricate a 1000x estimate error if filtering happened late.
	mustExec(t, ctx, pool, `INSERT INTO sage.explain_cache (queryid,captured_at,plan_json,source)
		VALUES ($1,now()-interval '1 hour','[{"Plan":{"Node Type":"Seq Scan","Plan Rows":4,"Actual Rows":8,"Actual Loops":1}}]','auto_explain')`, qid)
	mustExec(t, ctx, pool, `INSERT INTO sage.explain_cache (queryid,captured_at,plan_json,source)
		SELECT $1,now()-g*interval '1 second','[{"Plan":{"Node Type":"Seq Scan","Plan Rows":1,"Actual Rows":1000,"Actual Loops":1}}]'::jsonb,'lakebase_plan_history'
		FROM generate_series(1,20) g`, qid)
	src := NewPostgresObservationSource(pool)
	read := func() EstimateSample {
		t.Helper()
		got, err := src.EstimateErrors(ctx, []int64{qid}, time.Now().Add(-2*time.Hour), time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got.Plans != 1 || got.QError != 2 {
		t.Fatalf("only the local actual-row sample can verify an action: %+v", got)
	}
	mustExec(t, ctx, pool, "DELETE FROM sage.explain_cache WHERE queryid=$1 AND source='auto_explain'", qid)
	if got := read(); got.Plans != 0 {
		t.Fatalf("provider-only history must leave verification inconclusive: %+v", got)
	}
}
