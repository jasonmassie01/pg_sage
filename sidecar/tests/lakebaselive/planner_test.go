package lakebaselive

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/startup"
	"github.com/pg-sage/sidecar/internal/vectorlab"
)

func TestLakebasePlannerFeatures(t *testing.T) {
	p, ctx := livePool(t)
	for _, extension := range []string{"pg_stat_statements", "pg_hint_plan", "vector"} {
		exec(t, p, ctx, "CREATE EXTENSION IF NOT EXISTS "+extension)
	}
	exec(t, p, ctx, "CREATE SCHEMA IF NOT EXISTS sage_live")
	exec(t, p, ctx, "DROP TABLE IF EXISTS sage_live.planner_items")
	exec(t, p, ctx, "CREATE TABLE sage_live.planner_items(id int PRIMARY KEY,category int)")
	exec(t, p, ctx, "INSERT INTO sage_live.planner_items SELECT i,i%100 FROM generate_series(1,5000)i")
	exec(t, p, ctx, "CREATE INDEX planner_items_category_idx ON sage_live.planner_items(category)")
	exec(t, p, ctx, "ANALYZE sage_live.planner_items")
	t.Run("StartupPrerequisites", func(t *testing.T) {
		result, err := startup.RunChecks(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || result.PGVersionNum < 180000 || !result.QueryTextVisible {
			t.Fatalf("missing startup capabilities: %+v", result)
		}
		t.Logf("version=%d, query text visible, WAL columns=%v, plan columns=%v", result.PGVersionNum, result.HasWALColumns, result.HasPlanTimeColumns)
	})
	t.Run("GenericPlanAndOptimizerContext", func(t *testing.T) {
		query := collector.QueryStats{QueryID: 1, Calls: 100, Query: "SELECT id FROM sage_live.planner_items WHERE category=$1"}
		snap := &collector.Snapshot{Queries: []collector.QueryStats{query}, Tables: []collector.TableStats{{SchemaName: "sage_live", RelName: "planner_items", NLiveTup: 5000}}}
		cfg := config.DefaultConfig()
		cfg.LLM.Optimizer.PlanSource = "generic_plan"
		o := optimizer.New(p, &cfg.LLM.Optimizer, 180000, func(string, string, ...any) {})
		tc, ok, err := o.TableContext(ctx, snap, "sage_live.planner_items")
		if err != nil {
			t.Fatal(err)
		}
		if !ok || tc.Schema != "sage_live" || len(tc.Columns) != 2 || len(tc.Plans) != 1 || tc.PlanSource != "generic_plan" {
			t.Fatalf("real optimizer context missing: ok=%v columns=%d plans=%d source=%s", ok, len(tc.Columns), len(tc.Plans), tc.PlanSource)
		}
		t.Log("production optimizer read real columns/indexes/statistics and captured a generic parameterized EXPLAIN")
	})
	t.Run("HintHookCorrectness", func(t *testing.T) {
		conn, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		var loaded bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_settings WHERE name='pg_hint_plan.enable_hint')").Scan(&loaded); err != nil {
			t.Fatal(err)
		}
		if !loaded {
			if _, err := conn.Exec(ctx, "LOAD 'pg_hint_plan'"); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && (pgErr.Code == "42501" || pgErr.Code == "0A000") {
					caps := fleet.CollectProviderCapabilities(ctx, p, "lakebase", nil)
					if caps.Extensions["pg_hint_plan"] != "installed_not_loaded" {
						t.Fatalf("inactive hint hook must not be advertised ready: %s", caps.Extensions["pg_hint_plan"])
					}
					var mayPreload bool
					if err := conn.QueryRow(ctx, "SELECT has_parameter_privilege(current_user,'session_preload_libraries','SET')").Scan(&mayPreload); err != nil {
						t.Fatal(err)
					}
					t.Logf("UNSUPPORTED in current configuration: pg_hint_plan installed but inactive; LOAD SQLSTATE%s; SET session_preload_libraries privilege=%v; product correctly reports installed_not_loaded", pgErr.Code, mayPreload)
					return
				}
				t.Fatal(err)
			}
		}
		for hint, want := range map[string]string{"SeqScan(i)": "Seq Scan", "IndexScan(i planner_items_category_idx)": "Index Scan"} {
			var raw []byte
			if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) SELECT /*+ "+hint+" */ id FROM sage_live.planner_items AS i WHERE category=42").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct {
				Plan struct {
					NodeType string `json:"Node Type"`
				} `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 || plans[0].Plan.NodeType != want {
				t.Fatalf("hint %s did not force %s", hint, want)
			}
		}
		t.Log("live hint hook forced both sequential and index scan plans")
	})
	t.Run("HypoPGFailsClosed", func(t *testing.T) {
		h := optimizer.NewHypoPG(p, func(string, string, ...any) {})
		if h.IsAvailable(ctx) {
			t.Fatal("HypoPG unexpectedly available; expand native validation before claiming compatibility")
		}
		if _, err := h.Validate(ctx, optimizer.Recommendation{DDL: "CREATE INDEX candidate ON sage_live.planner_items(category)"}, []optimizer.QueryInfo{{QueryID: 1, Text: "SELECT id FROM sage_live.planner_items WHERE category=42"}}); err == nil {
			t.Fatal("missing HypoPG must reject what-if validation")
		}
		t.Log("production HypoPG path fails closed instead of inventing hypothetical benefit")
	})
	t.Run("ProductionVectorLabHNSW", func(t *testing.T) {
		exec(t, p, ctx, "DROP TABLE IF EXISTS sage_live.vectors")
		exec(t, p, ctx, "CREATE TABLE sage_live.vectors(id int PRIMARY KEY,embedding vector(2))")
		exec(t, p, ctx, "INSERT INTO sage_live.vectors SELECT i,('['||i||',0]')::vector FROM generate_series(1,2000)i")
		exec(t, p, ctx, "CREATE INDEX vectors_hnsw ON sage_live.vectors USING hnsw(embedding vector_l2_ops)")
		exec(t, p, ctx, "ANALYZE sage_live.vectors")
		manifest := vectorlab.Manifest{Schema: "sage_live", Table: "vectors", IDColumn: "id", VectorColumn: "embedding", Distance: "l2", K: 2, Repeats: 2, MinRecall: .95, MaxP95MS: 5000, StatementTimeoutMS: 10000, TotalTimeoutMS: 120000, Variants: []vectorlab.Variant{{Name: "balanced", EFSearch: 100, IterativeScan: "strict_order"}}, Queries: []vectorlab.Query{{ID: "a", Vector: []float64{1.12, .3}}, {ID: "b", Vector: []float64{12.12, .3}}, {ID: "c", Vector: []float64{31.12, .3}}}}
		result, err := vectorlab.Run(ctx, p, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if result.QueryCount != 3 || result.AutoApply || len(result.Variants) != 1 || result.Variants[0].MinRecall != 1 || result.Snapshot == "" {
			t.Fatalf("vector lab result missing recall/snapshot/safety evidence: %+v", result)
		}
		for _, query := range result.Variants[0].Queries {
			if query.TruthCount != 2 || len(query.IndexNames) == 0 {
				t.Fatal("vector lab did not compare exact baseline with HNSW")
			}
		}
		t.Log("production vector lab: 3 queries, HNSW, exact baseline, recall1.0, no automatic application")
	})
}
