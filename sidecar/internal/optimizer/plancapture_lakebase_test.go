package optimizer

import (
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"testing"
	"time"
)

func TestLakebasePlansRequireVerifiedScopeAndPreserveCaptureFlags(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	qid := int64(77553311)
	_, _ = pool.Exec(t.Context(), `DELETE FROM sage.explain_cache WHERE queryid=$1`, qid)
	t.Cleanup(func() { _, _ = pool.Exec(t.Context(), `DELETE FROM sage.explain_cache WHERE queryid=$1`, qid) })
	off := false
	captured := time.Now().Add(-time.Hour)
	_, err := pool.Exec(t.Context(), `INSERT INTO sage.explain_cache(queryid,query_text,plan_json,source,total_cost,captured_at,provider_scope_hash,provider_compute_id,provider_event_id,received_at,analyze_enabled,duration_source)
 VALUES ($1,'SELECT 1','[{"Plan":{"Node Type":"Index Scan","Total Cost":1}}]','lakebase_plan_history',1,$2,'verified-a','compute-a','plan-a',now(),$3,'provider_duration_ms'),
 ($1,'SELECT 1','[{"Plan":{"Node Type":"Seq Scan","Total Cost":99}}]','lakebase_plan_history',99,now(),'foreign','compute-b','foreign-newer',now(),true,'provider_duration_ms')`, qid, captured, off)
	if err != nil {
		t.Fatal(err)
	}
	scope := ""
	o := New(pool, &config.OptimizerConfig{}, 150000, func(string, string, ...any) {}, WithLakebasePlanScope(func() string { return scope }))
	queries := []collector.QueryStats{{QueryID: qid, Query: "SELECT 1"}}
	plans, source := o.planner.CapturePlans(t.Context(), queries)
	if len(plans) != 0 || source != "query_text_only" {
		t.Fatalf("unbound history used: %s %+v", source, plans)
	}
	scope = "verified-a"
	plans, source = o.planner.CapturePlans(t.Context(), queries)
	if source != "lakebase_plan_history" || len(plans) != 1 || plans[0].ScanType != "Index Scan" || plans[0].AnalyzeEnabled == nil || *plans[0].AnalyzeEnabled || plans[0].BuffersEnabled != nil || plans[0].ComputeID != "compute-a" {
		t.Fatalf("wrong resource/provenance: %s %+v", source, plans)
	}
	o.planner.providerRetention = 30 * time.Minute
	plans, source = o.planner.CapturePlans(t.Context(), queries)
	if len(plans) != 0 || source != "query_text_only" {
		t.Fatal("configured event-age limit waits for asynchronous erasure")
	}
	var stored int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.explain_cache WHERE provider_scope_hash='verified-a' AND queryid=$1`, qid).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("test did not cover still-stored expired payload: %d %v", stored, err)
	}
	o.planner.providerRetention = 0
	_, err = pool.Exec(t.Context(), `UPDATE sage.explain_cache SET captured_at=now()-interval '8 days' WHERE provider_scope_hash='verified-a' AND queryid=$1`, qid)
	if err != nil {
		t.Fatal(err)
	}
	plans, source = o.planner.CapturePlans(t.Context(), queries)
	if len(plans) != 0 || source != "query_text_only" {
		t.Fatal("expired history used")
	}
}
