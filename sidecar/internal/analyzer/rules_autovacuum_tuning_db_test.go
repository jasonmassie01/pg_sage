package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-02 on real PostgreSQL: a runtime without an LLM (no advisor, so
// no configuration snapshot) still honours a DBA's per-table tuning.

const avTuneFixtureSQL = `
CREATE SCHEMA rules02_av;
CREATE TABLE rules02_av.tuned (id int PRIMARY KEY, v int)
  WITH (autovacuum_vacuum_scale_factor = 0.01);
CREATE TABLE rules02_av.loose (id int PRIMARY KEY, v int)
  WITH (autovacuum_vacuum_scale_factor = 0.2);
CREATE TABLE rules02_av.disabled (id int PRIMARY KEY, v int)
  WITH (autovacuum_enabled = false, autovacuum_vacuum_scale_factor = 0.2);
CREATE TABLE rules02_av.untuned (id int PRIMARY KEY, v int);`

var avTuneTables = []string{"tuned", "loose", "disabled", "untuned"}

// createAvTuneFixture gives each table 1,000 live rows and 2,000 updates,
// enough churn for the rule at min rows 500, then flushes the statistics.
func createAvTuneFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS rules02_av CASCADE")
	})
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS rules02_av CASCADE")
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, avTuneFixtureSQL); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, tbl := range avTuneTables {
		q := "INSERT INTO rules02_av." + tbl + " SELECT g, 0 FROM generate_series(1, 1000) g;" +
			"UPDATE rules02_av." + tbl + " SET v = 1; UPDATE rules02_av." + tbl + " SET v = 2"
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("churn %s: %v", tbl, err)
		}
	}
	if err := testdb.FlushStats(ctx, conn.Conn()); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
}

// avTuneByTable runs the rule on snap and keys its fixture findings.
func avTuneByTable(snap *collector.Snapshot, minRows int) map[string]Finding {
	cfg := phase2Config()
	cfg.Analyzer.AutovacuumTuneMinRows = minRows
	out := map[string]Finding{}
	for _, f := range ruleAutovacuumTuning(snap, nil, cfg, nil) {
		out[f.ObjectIdentifier] = f
	}
	return out
}

func TestRuleAutovacuumTuning_NoLLMRuntimeHonoursDBATuning(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	createAvTuneFixture(t, ctx, pool)
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	cfg := phase2Config()
	cfg.Advisor.Enabled = true     // the default, but no LLM: no advisor runs
	cfg.Safety.CPUCeilingPct = 100 // a busy shared test server must not skip cycles
	coll := collector.New(pool, cfg, version, noopLog)
	coll.WithoutConfigSnapshots()

	for cycle := 1; cycle <= 2; cycle++ {
		snap := collectOnce(t, ctx, coll)
		if snap.ConfigData != nil {
			t.Fatalf("cycle %d: ConfigData collected on a no-LLM runtime", cycle)
		}
		got := avTuneByTable(snap, 500)
		for _, skipped := range []string{"rules02_av.tuned", "rules02_av.disabled"} {
			if f, ok := got[skipped]; ok {
				t.Errorf("cycle %d: %s recommended: %s", cycle, skipped, f.RecommendedSQL)
			}
		}
		loose, ok := got["rules02_av.loose"]
		if !ok || loose.RollbackSQL !=
			`ALTER TABLE "rules02_av"."loose" SET (autovacuum_vacuum_scale_factor = 0.2);` {
			t.Errorf("cycle %d: loose finding %+v, want a 0.05 SET restoring 0.2", cycle, loose)
		}
		untuned, ok := got["rules02_av.untuned"]
		if !ok {
			t.Fatalf("cycle %d: untuned churning table not flagged: setup broken", cycle)
		}
		if cycle == 2 { // pg_sage applies its own recommendation
			if _, err := pool.Exec(ctx, untuned.RecommendedSQL); err != nil {
				t.Fatalf("apply %q: %v", untuned.RecommendedSQL, err)
			}
		}
	}
	after := avTuneByTable(collectOnce(t, ctx, coll), 500)
	if f, ok := after["rules02_av.untuned"]; ok {
		t.Fatalf("rule re-fired after pg_sage's own SET: %s", f.RecommendedSQL)
	}
}

// collectOnce runs the collector until it publishes a snapshot newer than
// the one it holds now.
func collectOnce(t *testing.T, ctx context.Context, coll *collector.Collector) *collector.Snapshot {
	t.Helper()
	before := coll.LatestSnapshot()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { coll.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if snap := coll.LatestSnapshot(); snap != nil && snap != before {
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("collector produced no new snapshot within 30s")
	return nil
}
