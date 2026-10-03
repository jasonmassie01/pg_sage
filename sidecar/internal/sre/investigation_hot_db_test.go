package sre

import (
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offender: sage.sre_investigations had 43 % HOT updates.
// Every lease grant, step, budget and case update moves updated_at, and
// two indexes keyed it (the queue and the retention index). With no index
// on updated_at, an update that moves only it and unindexed columns is
// heap-only (perf-selfexcl).
func TestInvestigationProgressUpdateIsHeapOnly(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, _, err := st.Create(ctx, lockStart(scope, "hot-update"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for range 3 {
		if _, err := tx.Exec(ctx, `UPDATE sage.sre_investigations
			SET active_ms = active_ms + 1, probe_count = probe_count + 1,
			    updated_at = clock_timestamp()
			WHERE deployment_id = $1 AND database_id = $2 AND id = $3`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(inv.ID)); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	var upd, hot int64
	if err := tx.QueryRow(ctx, `SELECT n_tup_upd, n_tup_hot_upd FROM pg_stat_xact_user_tables
		WHERE relid = 'sage.sre_investigations'::regclass`).Scan(&upd, &hot); err != nil {
		t.Fatal(err)
	}
	if upd != 3 || hot != 3 {
		t.Fatalf("3 progress updates: %d updates, %d HOT; want all heap-only", upd, hot)
	}
}

// Without the updated_at index, the retention read is an index range on
// created_at (updated_at >= created_at, so an investigation unchanged
// since the cutoff was also created before it), in the generic plan too.
func TestInvestigationRetentionReadIsAnIndexRange(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	seedListHistory(t, pool, scope)
	plans, err := perfgate.ExplainStatements(ctx, pool, []perfgate.Statement{
		{Query: agedIDsSQL("")}, {Query: agedIDsSQL(keepHeldBudget)}})
	if errors.Is(err, perfgate.ErrGenericPlanUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Err != "" || len(p.SeqScans) != 0 {
			t.Errorf("retention read: seq scans %v, err %q", p.SeqScans, p.Err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids, err := agedIDs(ctx, tx, scope, 50*time.Minute, 10, "")
	if err != nil || len(ids) != 10 {
		t.Fatalf("aged ids = %d (%v), want a full batch of 10", len(ids), err)
	}
}
