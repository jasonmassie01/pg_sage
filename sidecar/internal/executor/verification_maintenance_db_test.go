package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Phase 1.3 integration tests for maintenance actions: VACUUM and VACUUM
// FREEZE are judged by the table metric they were predicted to move.

// VACUUM is verified by the dead tuples it was predicted to remove, not
// marked successful the moment it returns.
func TestOutcome_VacuumVerifiedByDeadTuples(t *testing.T) {
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("vo_vac_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+
		" AS SELECT g AS a FROM generate_series(1, 4000) g"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	deleteAndFlush(t, ctx, pool, "DELETE FROM public."+table+" WHERE a > 1000")
	waitForDeadTuples(t, ctx, pool, table)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := "VACUUM public." + table
	before := map[string]any{}
	p := exec.predictAction(ctx, sql, nil, before)
	if p.Baseline == nil || *p.Baseline < 1000 {
		t.Fatalf("vacuum prediction baseline = %v, want the dead tuples", p.Baseline)
	}
	raw, _ := json.Marshal(before)
	id := insertVerifiedAction(t, pool, verifiedActionRow{sql: sql, before: string(raw),
		executedAt: time.Now().UTC()})
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	exec.verifyImmediate(ctx, id)

	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved || got.Observed.ChangePct == nil ||
		*got.Observed.ChangePct > -50 {
		t.Fatalf("vacuum outcome = %+v, want improved with dead tuples removed", got)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success", outcome)
	}
}

func deleteAndFlush(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	// PG15+ flushes pending stats on demand; older servers flush within
	// a second of the transaction ending.
	_, _ = conn.Exec(ctx, "SELECT pg_stat_force_next_flush()")
	_, _ = conn.Exec(ctx, "SELECT 1")
}

func waitForDeadTuples(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var dead int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(n_dead_tup, 0) FROM
			pg_stat_user_tables WHERE relid = to_regclass($1)`, "public."+table).
			Scan(&dead); err == nil && dead >= 1000 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dead tuples on %s never reached the statistics", table)
}

// VACUUM (FREEZE) is judged by the relfrozenxid age it was run to
// advance, not by dead tuples (a freshly loaded table has none).
func TestOutcome_VacuumFreezeVerifiedByXIDAge(t *testing.T) {
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("vo_frz_%d", time.Now().UnixNano())
	for _, sql := range []string{"CREATE TABLE public." + table + " (a int)",
		"INSERT INTO public." + table + " SELECT generate_series(1, 100)"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// Age the table: right after creation its relfrozenxid age is ~5, so a
	// freeze can only move it by a few and is judged "barely moved" (CI on
	// PR #120: 5 -> 4, neutral). Each statement is its own transaction.
	for i := 0; i < 200; i++ {
		if _, err := pool.Exec(ctx, "SELECT txid_current()"); err != nil {
			t.Fatalf("consume xid: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := "VACUUM (FREEZE) public." + table
	before := map[string]any{}
	p := exec.predictAction(ctx, sql, nil, before)
	if p.Metric != verify.MetricFrozenXIDAge || p.Baseline == nil || *p.Baseline < 1 {
		t.Fatalf("freeze prediction = %+v, want a relfrozenxid age baseline", p)
	}
	raw, _ := json.Marshal(before)
	id := insertVerifiedAction(t, pool, verifiedActionRow{sql: sql, before: string(raw),
		executedAt: time.Now().UTC()})
	waitXminHorizonPast(t, ctx, pool, "public."+table)
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("vacuum freeze: %v", err)
	}

	exec.verifyImmediate(ctx, id)

	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved || got.Observed.Metric != verify.MetricFrozenXIDAge {
		t.Fatalf("freeze outcome = %+v, want improved on relfrozenxid age", got)
	}
}

// waitXminHorizonPast waits until nothing that holds the xmin horizon (other
// backends' snapshots, replication slots' xmin, prepared transactions) is
// older than the table's relfrozenxid. VACUUM FREEZE can only advance relfrozenxid to
// the oldest running xmin, and on the shared CI server another package's
// open transaction held it behind the new table, so the age could not drop
// (PG15 CI: neutral instead of improved).
func waitXminHorizonPast(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) {
	t.Helper()
	for i := 0; i < 120; i++ {
		var clear bool
		if err := pool.QueryRow(ctx, `WITH c AS (SELECT relfrozenxid FROM pg_class
			WHERE oid = to_regclass($1))
		SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity a, c
			WHERE a.pid <> pg_backend_pid() AND a.backend_xmin IS NOT NULL
			  AND age(a.backend_xmin) >= age(c.relfrozenxid))
		AND NOT EXISTS (SELECT 1 FROM pg_replication_slots s, c
			WHERE s.xmin IS NOT NULL AND age(s.xmin) >= age(c.relfrozenxid))
		AND NOT EXISTS (SELECT 1 FROM pg_prepared_xacts p, c
			WHERE age(p.transaction) >= age(c.relfrozenxid))`, table).Scan(&clear); err != nil {
			t.Fatalf("read the xmin horizon: %v", err)
		}
		if clear {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("another backend held the xmin horizon behind %s for 30 s", table)
}
