package executor

import (
	"context"
	"testing"
	"time"
)

func TestPlanReferencedTargetsExcludeUnboundProviderHistory(t *testing.T) {
	pool, ctx := requireDB(t)
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid IN ($1,$2)", qid, qid+1)
	})
	_, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (queryid,captured_at,plan_json,source) VALUES
		($1,now()-interval '1 hour','[{"Plan":{"Node Type":"Index Scan","Index Name":"legacy_target_idx"}}]','auto_explain'),
		($2,now()-interval '1 hour','[{"Plan":{"Node Type":"Seq Scan"}}]','auto_explain')`, qid, qid+1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.explain_cache (queryid,captured_at,plan_json,source)
		SELECT q,now()-g*interval '1 second','[{"Plan":{"Node Type":"Index Scan","Index Name":"provider_only_idx"}}]'::jsonb,'lakebase_plan_history'
		FROM unnest($1::bigint[]) q CROSS JOIN generate_series(1,4) g`, []int64{qid, qid + 1})
	if err != nil {
		t.Fatal(err)
	}
	read := func(index string) []int64 {
		t.Helper()
		rows, err := pool.Query(ctx, planReferencedSQL, []int64{qid, qid + 1}, index)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := read("legacy_target_idx"); len(got) != 1 || got[0] != qid {
		t.Fatalf("newer provider rows displaced the local drop-target evidence: %v", got)
	}
	if got := read("provider_only_idx"); len(got) != 0 {
		t.Fatalf("asynchronous provider history invented drop-index targets: %v", got)
	}
}
