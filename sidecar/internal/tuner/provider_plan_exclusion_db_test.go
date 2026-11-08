package tuner

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFetchPlanJSONExcludesUnboundProviderHistory(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid=$1", qid)
	})
	_, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, captured_at, plan_json, source) VALUES
		($1, now()-interval '1 hour', '[{"Plan":{"Node Type":"Index Scan"}}]', 'auto_explain'),
		($1, now(), '[{"Plan":{"Node Type":"Seq Scan"}}]', 'lakebase_plan_history')`, qid)
	if err != nil {
		t.Fatal(err)
	}
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	if got := tu.fetchPlanJSON(ctx, qid); !strings.Contains(got, "Index Scan") {
		t.Fatalf("newer provider row displaced local plan: %s", got)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM sage.explain_cache WHERE queryid=$1 AND source='auto_explain'", qid); err != nil {
		t.Fatal(err)
	}
	if got := tu.fetchPlanJSON(ctx, qid); got != "" {
		t.Fatalf("provider-only history must not supply tuning evidence: %s", got)
	}
}
