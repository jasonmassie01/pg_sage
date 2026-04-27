// Package smoke contains end-to-end smoke tests that thread data
// through multiple real subsystems (collector-style snapshot →
// analyzer persistence → retention purge) against a live
// PostgreSQL instance.
//
// These tests skip cleanly when SAGE_DATABASE_URL is unset.
package smoke

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/schema"
)

// ================================================================
// Test infrastructure.
// ================================================================

var (
	sPool     *pgxpool.Pool
	sPoolOnce sync.Once
	sPoolErr  error
)

func smokeDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
}

func requireSmokeDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()

	sPoolOnce.Do(func() {
		dsn := smokeDSN()
		qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		pool, err := pgxpool.New(qctx, dsn)
		if err != nil {
			sPoolErr = fmt.Errorf("pgxpool.New: %w", err)
			return
		}
		if err := pool.Ping(qctx); err != nil {
			pool.Close()
			sPoolErr = fmt.Errorf("ping: %w", err)
			return
		}
		if err := schema.Bootstrap(qctx, pool); err != nil {
			pool.Close()
			sPoolErr = fmt.Errorf("bootstrap: %w", err)
			return
		}
		// Release the lock Bootstrap held so other tests proceed.
		schema.ReleaseAdvisoryLock(qctx, pool)

		sPool = pool
	})

	if sPoolErr != nil {
		t.Skipf("smoke DB unavailable: %v", sPoolErr)
	}
	return sPool, ctx
}

// silentLog discards log output in tests.
func silentLog(string, string, ...any) {}

// debugLog returns a logger that routes collector/analyzer log
// lines to t.Logf, keyed off the test.
func debugLog(t *testing.T) func(string, string, ...any) {
	return func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}
}

// ================================================================
// CHECK-01..08: full-stack smoke test.
//
// Stages:
//   1. collector produces a Snapshot against a real DB
//   2. findings are persisted via analyzer.UpsertFindings
//   3. ResolveCleared transitions stale findings to resolved
//   4. retention.Cleaner purges expired snapshot rows
// ================================================================

func TestSmoke_EndToEndPipeline(t *testing.T) {
	pool, ctx := requireSmokeDB(t)

	// The two object_identifiers this test upserts. Assertions below
	// filter to just these rows so concurrent integration tests in
	// other packages (analyzer, executor, briefing, …) that also write
	// to sage.findings don't corrupt our counts.
	smokeOwnedFindings := []string{"public.idx_smoke_a", "public.t_smoke"}
	const smokeOwnedFilter = `object_identifier = ANY($1)`

	// Clean only our test's prior rows. Other tables are scoped to this
	// test's run so a full-table DELETE is safe for them.
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.findings WHERE "+smokeOwnedFilter,
		smokeOwnedFindings); err != nil {
		t.Logf("clean smoke findings: %v", err)
	}
	for _, tbl := range []string{"sage.snapshots", "sage.action_log"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Logf("clean %s (may not exist): %v", tbl, err)
		}
	}

	// ---- CHECK-01: collector produces a non-nil Snapshot. ----
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 1
	cfg.Safety.CPUCeilingPct = 100 // never trip the breaker
	cfg.Retention.SnapshotsDays = 7
	cfg.Retention.FindingsDays = 7
	cfg.Retention.ActionsDays = 7
	cfg.Retention.ExplainsDays = 7

	// Discover PG version for the collector.
	var pgVerNum int
	if err := pool.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").
		Scan(&pgVerNum); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}

	coll := collector.New(pool, cfg, pgVerNum, debugLog(t))

	// Run collector briefly — it runs collect() on each tick.
	collCtx, collCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		coll.Run(collCtx)
		close(done)
	}()

	// Wait for at least one in-memory snapshot AND one persisted row.
	// Cancelling the collector context before persist finishes was causing
	// "context already done: context canceled" from the persist query, and
	// a cascading CHECK-06 failure (no snapshots to backdate).
	var snap *collector.Snapshot
	var snapRows int
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if snap == nil {
			if s := coll.LatestSnapshot(); s != nil {
				snap = s
			}
		}
		if snap != nil {
			if err := pool.QueryRow(ctx,
				"SELECT COUNT(*) FROM sage.snapshots").Scan(&snapRows); err == nil {
				if snapRows > 0 {
					break
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	collCancel()
	<-done

	if snap == nil {
		t.Fatal("CHECK-01 FAIL: collector did not produce a snapshot")
	}
	if snap.CollectedAt.IsZero() {
		t.Error("CHECK-01 FAIL: Snapshot.CollectedAt is zero")
	}
	t.Logf("CHECK-01 PASS: snapshot collected, %d tables %d indexes",
		len(snap.Tables), len(snap.Indexes))

	// ---- CHECK-02: snapshots table has rows from persist. ----
	if snapRows == 0 {
		// Poll once more after cancel settles, in case persist raced.
		if err := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM sage.snapshots").Scan(&snapRows); err != nil {
			t.Fatalf("count snapshots: %v", err)
		}
	}
	if snapRows == 0 {
		t.Fatal("CHECK-02 FAIL: collector did not persist any snapshot rows")
	}
	t.Logf("CHECK-02 PASS: %d snapshot rows persisted", snapRows)

	// ---- CHECK-03: analyzer persists findings via UpsertFindings. ----
	findings := []analyzer.Finding{
		{
			Category:         "index_health",
			Severity:         "warning",
			ObjectType:       "index",
			ObjectIdentifier: "public.idx_smoke_a",
			Title:            "smoke unused index",
			Detail:           map[string]any{"scans": 0},
			Recommendation:   "Drop it",
			RecommendedSQL:   "DROP INDEX public.idx_smoke_a",
		},
		{
			Category:         "vacuum",
			Severity:         "info",
			ObjectType:       "table",
			ObjectIdentifier: "public.t_smoke",
			Title:            "dead tuples",
			Detail:           map[string]any{"dead": 10},
			Recommendation:   "vacuum",
		},
	}
	if err := analyzer.UpsertFindings(ctx, pool, findings); err != nil {
		t.Fatalf("UpsertFindings: %v", err)
	}

	// ---- CHECK-04: upsert increments occurrence_count. ----
	// Re-upsert the first finding — count should become 2.
	if err := analyzer.UpsertFindings(
		ctx, pool, findings[:1]); err != nil {
		t.Fatalf("second UpsertFindings: %v", err)
	}
	var occ int
	if err := pool.QueryRow(ctx,
		`SELECT occurrence_count FROM sage.findings
		 WHERE object_identifier = $1 AND status = 'open'`,
		"public.idx_smoke_a").Scan(&occ); err != nil {
		t.Fatalf("query occ: %v", err)
	}
	if occ != 2 {
		t.Errorf("CHECK-04 FAIL: occurrence_count=%d, want 2", occ)
	} else {
		t.Logf("CHECK-04 PASS: occurrence_count incremented to %d", occ)
	}

	// ---- CHECK-05: ResolveCleared resolves missing finding IDs. ----
	// Keep only one — the other should be resolved.
	active := map[string]bool{
		"public.idx_smoke_a": true,
	}
	if err := analyzer.ResolveCleared(
		ctx, pool, active, "index_health"); err != nil {
		t.Fatalf("ResolveCleared: %v", err)
	}
	// Category "vacuum" is untouched because ResolveCleared only
	// touches the category passed in. Run it again to resolve
	// vacuum findings with an empty active set.
	if err := analyzer.ResolveCleared(
		ctx, pool, map[string]bool{}, "vacuum"); err != nil {
		t.Fatalf("ResolveCleared vacuum: %v", err)
	}

	var openCount, resolvedCount int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM sage.findings WHERE status='open' AND "+
			smokeOwnedFilter, smokeOwnedFindings).
		Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM sage.findings WHERE status='resolved' AND "+
			smokeOwnedFilter, smokeOwnedFindings).
		Scan(&resolvedCount); err != nil {
		t.Fatalf("count resolved: %v", err)
	}
	if openCount != 1 {
		t.Errorf("CHECK-05 FAIL: open count=%d, want 1", openCount)
	}
	if resolvedCount != 1 {
		t.Errorf("CHECK-05 FAIL: resolved count=%d, want 1",
			resolvedCount)
	}
	t.Logf("CHECK-05 PASS: open=%d resolved=%d", openCount, resolvedCount)

	// ---- CHECK-06: retention purges expired snapshots. ----
	// Backdate one snapshot row by 100 days; it should be purged.
	tag, err := pool.Exec(ctx,
		`UPDATE sage.snapshots
		 SET collected_at = now() - interval '100 days'
		 WHERE ctid = (SELECT ctid FROM sage.snapshots LIMIT 1)`)
	if err != nil {
		t.Fatalf("backdate snapshot: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected 1 backdate, got %d", tag.RowsAffected())
	}

	var beforePurge int
	pool.QueryRow(ctx, "SELECT COUNT(*) FROM sage.snapshots").
		Scan(&beforePurge)

	cleaner := retention.New(pool, cfg, silentLog)
	cleaner.Run(ctx)

	var afterPurge int
	pool.QueryRow(ctx, "SELECT COUNT(*) FROM sage.snapshots").
		Scan(&afterPurge)
	if afterPurge >= beforePurge {
		t.Errorf("CHECK-06 FAIL: retention did not purge "+
			"(before=%d, after=%d)", beforePurge, afterPurge)
	} else {
		t.Logf("CHECK-06 PASS: snapshots purged %d → %d",
			beforePurge, afterPurge)
	}

	// ---- CHECK-07: retention preserves resolved findings within
	// the retention window (last_seen is recent). ----
	var resolvedRemaining int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sage.findings
		 WHERE status='resolved' AND `+smokeOwnedFilter,
		smokeOwnedFindings).Scan(&resolvedRemaining)
	if resolvedRemaining != 1 {
		t.Errorf("CHECK-07 FAIL: recent resolved findings "+
			"should be preserved, got %d", resolvedRemaining)
	} else {
		t.Logf("CHECK-07 PASS: recent resolved findings preserved")
	}

	// ---- CHECK-08: retention purges resolved findings past TTL. ----
	_, err = pool.Exec(ctx,
		`UPDATE sage.findings
		 SET last_seen = now() - interval '100 days'
		 WHERE status = 'resolved' AND `+smokeOwnedFilter,
		smokeOwnedFindings)
	if err != nil {
		t.Fatalf("backdate finding: %v", err)
	}
	cleaner.Run(ctx)

	var resolvedAfter int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sage.findings
		 WHERE status='resolved' AND `+smokeOwnedFilter,
		smokeOwnedFindings).Scan(&resolvedAfter)
	if resolvedAfter != 0 {
		t.Errorf("CHECK-08 FAIL: expired resolved findings "+
			"not purged (remaining=%d)", resolvedAfter)
	} else {
		t.Logf("CHECK-08 PASS: expired resolved findings purged")
	}
}
