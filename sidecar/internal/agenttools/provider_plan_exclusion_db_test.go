package agenttools

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestAttachPlansExcludesUnboundProviderHistory(t *testing.T) {
	f := newFixture(t)
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid IN ($1,$2)", qid, qid+1)
	})
	f.execArgs(`INSERT INTO sage.explain_cache (queryid,captured_at,plan_json,source) VALUES
		($1,now()-interval '1 hour','[{"Plan":{"Node Type":"Index Scan"}}]','auto_explain'),
		($1,now(),'[{"Plan":{"Node Type":"Foreign Scan"}}]','lakebase_plan_history'),
		($2,now(),'[{"Plan":{"Node Type":"Foreign Scan"}}]','lakebase_plan_history')`, qid, qid+1)
	queries := []TopQuery{{QueryID: QueryID(qid)}, {QueryID: QueryID(qid + 1)}}
	if err := New(f.pool, Options{}).attachPlans(f.ctx, queries); err != nil {
		t.Fatal(err)
	}
	if p := queries[0].Plan; p == nil || p.Source != "auto_explain" || !slices.Contains(p.NodeTypes, "Index Scan") {
		t.Fatalf("local cached plan must be attached: %+v", p)
	}
	if queries[1].Plan != nil {
		t.Fatalf("provider-only history must not be attached as local evidence: %+v", queries[1].Plan)
	}
}
