package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rca-04: the on-demand EXPLAIN (no ANALYZE) was stored as source
// 'auto_explain' although auto_explain never saw it. The migration renames
// those rows to 'explain_estimate' and leaves every other source alone. No
// concurrent-writer test: Bootstrap holds its advisory lock, so one
// migration runs per database at a time.

func seedExplain(t *testing.T, pool *pgxpool.Pool, qid int64, source string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.explain_cache
		(queryid, plan_json, source) VALUES ($1, '[{"Plan":{"Total Cost":1}}]', $2)`,
		qid, source); err != nil {
		t.Fatalf("seed %s plan: %v", source, err)
	}
}

func explainSources(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT source, count(*) FROM sage.explain_cache GROUP BY source`)
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var source string
		var n int
		if err := rows.Scan(&source, &n); err != nil {
			t.Fatalf("scan sources: %v", err)
		}
		got[source] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sources: %v", err)
	}
	return got
}

func TestExplainEstimateSource_RenamesOldRowsOnce(t *testing.T) {
	pool := freshSchemaPool(t, "explainest")
	ctx := context.Background()
	seedExplain(t, pool, 1, "auto_explain")
	seedExplain(t, pool, 2, "auto_explain")
	seedExplain(t, pool, 3, "auto_explain_log")
	seedExplain(t, pool, 4, "generic_plan")
	seedExplain(t, pool, 5, "lakebase_plan_history")
	for run := range 2 { // the second run is a no-op
		if _, err := pool.Exec(ctx, ddlExplainEstimateSource); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got := explainSources(t, pool)
		want := map[string]int{"explain_estimate": 2, "auto_explain_log": 1,
			"generic_plan": 1, "lakebase_plan_history": 1}
		if len(got) != len(want) {
			t.Fatalf("run %d: sources = %v, want %v", run, got, want)
		}
		for source, n := range want {
			if got[source] != n {
				t.Fatalf("run %d: sources = %v, want %v", run, got, want)
			}
		}
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap after the migration: %v", err)
	}
}

// Bootstrap itself runs the step on a database that already holds old rows.
func TestBootstrap_RenamesTheEstimateSource(t *testing.T) {
	pool := freshSchemaPool(t, "explainestboot")
	seedExplain(t, pool, 7, "auto_explain")
	if err := Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := explainSources(t, pool); got["explain_estimate"] != 1 || got["auto_explain"] != 0 {
		t.Fatalf("sources after bootstrap = %v, want one explain_estimate row", got)
	}
}

// Empty: on a cache with no rows the step changes nothing and succeeds.
func TestExplainEstimateSource_EmptyCache(t *testing.T) {
	pool := freshSchemaPool(t, "explainestempty")
	if _, err := pool.Exec(context.Background(), ddlExplainEstimateSource); err != nil {
		t.Fatalf("empty cache: %v", err)
	}
	if got := explainSources(t, pool); len(got) != 0 {
		t.Fatalf("sources = %v, want none", got)
	}
}

// The step is bounded and is a tranche-two step.
func TestExplainEstimateSource_IsBoundedAndRegistered(t *testing.T) {
	if !strings.Contains(ddlExplainEstimateSource, "SET LOCAL lock_timeout") {
		t.Fatal("the rename must run under a lock_timeout")
	}
	if !strings.Contains(ddlExplainEstimateSource, "source = 'auto_explain'") {
		t.Fatal("the rename must touch only source = 'auto_explain' rows")
	}
	if !isTrancheTwoStep("ddlExplainEstimateSource") {
		t.Fatalf("ddlExplainEstimateSource is not a tranche-two step: %v", trancheTwoMigrations())
	}
}

// isTrancheTwoStep reports whether name is one of trancheTwoMigrations.
func isTrancheTwoStep(name string) bool {
	for _, s := range trancheTwoMigrations() {
		if s[0] == name {
			return true
		}
	}
	return false
}
