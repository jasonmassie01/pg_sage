//go:build providerlive

package providerlive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

func checkFeatures(t *testing.T, pool *pgxpool.Pool, provider target) {
	f := newFixture(t, pool, provider)
	f.exec(t, "CREATE TABLE "+f.table("items")+" (id int PRIMARY KEY, category int)")
	f.exec(t, "INSERT INTO "+f.table("items")+
		" SELECT i, i % 100 FROM generate_series(1,2000) i")
	f.exec(t, "ANALYZE "+f.table("items"))
	t.Run("concurrent_index_and_vacuum", func(t *testing.T) { checkDDL(t, f) })
	t.Run("collector_persistence", func(t *testing.T) { checkCollector(t, f) })
	t.Run("optimizer_context_and_plan", func(t *testing.T) { checkOptimizer(t, f) })
	t.Run("hypopg_session_evidence", func(t *testing.T) { checkHypoPG(t, f) })
	t.Run("hint_execution", func(t *testing.T) { checkHints(t, f) })
	t.Run("vector_lab", func(t *testing.T) { checkVector(t, f) })
}

func checkDDL(t *testing.T, f fixture) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	index := f.table("items_category_idx")
	sql := "CREATE INDEX CONCURRENTLY items_category_idx ON " + f.table("items") + " (category)"
	checkError(t, "create concurrent index",
		executor.ExecConcurrently(ctx, f.pool, sql, 30*time.Second))
	var valid bool
	checkError(t, "verify real index", f.pool.QueryRow(ctx,
		"SELECT indisvalid FROM pg_catalog.pg_index WHERE indexrelid=$1::regclass",
		index).Scan(&valid))
	if !valid {
		t.Fatal("created index is invalid")
	}
	f.exec(t, "DELETE FROM "+f.table("items")+" WHERE id > 1900")
	checkError(t, "vacuum outside transaction", executor.ExecConcurrently(ctx, f.pool,
		"VACUUM (ANALYZE) "+f.table("items"), 30*time.Second))
	var count int
	checkError(t, "verify vacuum preserves remaining rows",
		f.pool.QueryRow(ctx, "SELECT count(*) FROM "+f.table("items")).Scan(&count))
	if count != 1900 {
		t.Fatalf("unexpected synthetic row count: %d", count)
	}
	if err := executor.ExecConcurrently(ctx, f.pool,
		"CREATE INDEX CONCURRENTLY broken ON "+f.table("missing")+" (id)",
		30*time.Second); err == nil {
		t.Fatal("DDL against missing relation unexpectedly succeeded")
	}
}

func checkCollector(t *testing.T, f fixture) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 1
	cfg.Advisor.Enabled = true
	var version int
	checkError(t, "read collector version", f.pool.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&version))
	c := collector.New(f.pool, cfg, version, collectorLog(t, ctx))
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("collector did not persist and observe synthetic table within 45 seconds")
		case <-ticker.C:
			snapshot := c.LatestSnapshot()
			if hasFixtureTable(snapshot, f.namespace) && collectorPersisted(t, ctx, f) {
				checkSettingContexts(t, snapshot)
				return
			}
		}
	}
}

func collectorPersisted(t *testing.T, ctx context.Context, f fixture) bool {
	t.Helper()
	var snapshots, samples int
	checkError(t, "read persisted collector snapshots", f.pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.snapshots").Scan(&snapshots))
	checkError(t, "read durable query metrics", f.pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.query_store").Scan(&samples))
	return snapshots > 0 && samples > 0
}

func collectorLog(t *testing.T, ctx context.Context) func(string, string, ...any) {
	return func(level, format string, args ...any) {
		if level != "WARN" && level != "ERROR" {
			return
		}
		for _, arg := range args {
			if err, ok := arg.(error); ok {
				if ctx.Err() != nil && errors.Is(err, context.Canceled) {
					t.Log("collector in-flight work cancelled during fixture shutdown")
					return
				}
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) {
					t.Logf("collector SQLSTATE=%s", pgErr.Code)
				}
			}
		}
		t.Logf("collector %s: %s (arguments suppressed)", level, format)
	}
}

func checkSettingContexts(t *testing.T, snapshot *collector.Snapshot) {
	t.Helper()
	if snapshot.ConfigData == nil {
		t.Fatal("collector omitted configuration capability data")
	}
	contexts := make(map[string]string)
	for _, setting := range snapshot.ConfigData.PGSettings {
		contexts[setting.Name] = setting.Context
	}
	if contexts["work_mem"] != "user" || contexts["shared_buffers"] != "postmaster" {
		t.Fatal("collector lost session-tunable versus restart-required setting contexts")
	}
}

func hasFixtureTable(snap *collector.Snapshot, namespace string) bool {
	if snap == nil {
		return false
	}
	for _, table := range snap.Tables {
		if table.SchemaName == namespace && table.RelName == "items" && table.NLiveTup > 0 {
			return true
		}
	}
	return false
}

func checkOptimizer(t *testing.T, f fixture) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var version int
	checkError(t, "read optimizer version", f.pool.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&version))
	query := collector.QueryStats{QueryID: 1, Calls: 100,
		Query: "SELECT id FROM " + f.namespace + ".items WHERE category = $1"}
	snap := &collector.Snapshot{Queries: []collector.QueryStats{query},
		Tables: []collector.TableStats{{SchemaName: f.namespace, RelName: "items", NLiveTup: 1900}}}
	planner := optimizer.NewPlanCapture(f.pool, version, false, false, "generic_plan",
		func(string, string, ...any) {})
	contexts, source, err := optimizer.BuildTableContexts(ctx, f.pool, snap, planner, 1)
	checkError(t, "build real optimizer context", err)
	if len(contexts) != 1 || contexts[0].Schema != f.namespace ||
		len(contexts[0].Columns) != 2 || len(contexts[0].Queries) != 1 {
		t.Fatal("optimizer omitted synthetic table, columns, or query evidence")
	}
	if version >= 160000 && (source != "generic_plan" || len(contexts[0].Plans) != 1) {
		t.Fatal("PostgreSQL 16+ generic plan evidence missing")
	}
	t.Logf("optimizer plan source=%s", source)
}
