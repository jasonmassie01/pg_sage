package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSortWithoutIndexExcludesUnboundProviderHistory(t *testing.T) {
	pool := phase2Pool(t)
	a, _ := recordingAnalyzer(t, pool, 7)
	ctx := context.Background()
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid IN ($1,$2)", qid, qid+1)
	})
	sortPlan := `[{"Plan":{"Node Type":"Limit","Plan Rows":1,"Plans":[{"Node Type":"Sort","Plan Rows":1000,"Sort Key":["id"]}]}}]`
	_, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (queryid,captured_at,query_text,plan_json,source) VALUES
		($1,now()-interval '1 hour','local sort',$3::jsonb,'auto_explain'),
		($1,now(),'provider no sort','[{"Plan":{"Node Type":"Index Scan"}}]','lakebase_plan_history'),
		($2,now(),'provider sort',$3::jsonb,'lakebase_plan_history')`, qid, qid+1, sortPlan)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range a.checkSortWithoutIndex(ctx) {
		found[f.ObjectIdentifier] = true
	}
	if !found[fmt.Sprintf("queryid:%d", qid)] {
		t.Fatal("newer provider row displaced the local sort finding")
	}
	if found[fmt.Sprintf("queryid:%d", qid+1)] {
		t.Fatal("provider-only history invented a local sort finding")
	}
}
