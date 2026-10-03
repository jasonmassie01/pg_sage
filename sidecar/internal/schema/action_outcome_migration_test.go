package schema

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Phase 1.3: sage.action_outcome holds predicted vs observed per action.
// The migration is idempotent, checks the catalog before building its
// index, and a re-run takes no lock on sage.action_log.

func TestActionOutcomeMigration_CreatesTableAndIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass(
		'sage.idx_action_outcome_class_decided'))`).Scan(&def); err != nil || def == "" {
		t.Fatalf("index missing: %v", err)
	}
	if !strings.Contains(def, "(action_class, decided_at DESC)") {
		t.Fatalf("index = %s", def)
	}
	var cascade string
	if err := pool.QueryRow(ctx, `SELECT confdeltype FROM pg_constraint
		WHERE conrelid = 'sage.action_outcome'::regclass AND contype = 'f'`).
		Scan(&cascade); err != nil || cascade != "c" {
		t.Fatalf("foreign key on delete = %q (%v), want cascade with the action", cascade,
			err)
	}
}

func TestActionOutcomeMigration_RejectsUnknownVerdicts(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('drop_index', 'DROP INDEX CONCURRENTLY public.mig_probe') RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	for _, verdict := range []string{"success", "IMPROVED", ""} {
		_, err := pool.Exec(ctx, `INSERT INTO sage.action_outcome
			(action_log_id, action_class, predicted, prediction_method, verdict)
			VALUES ($1, 'index_drop', '{}', 'rule', $2)`, id, verdict)
		if err == nil {
			t.Errorf("verdict %q accepted", verdict)
			_, _ = pool.Exec(ctx, "DELETE FROM sage.action_outcome WHERE action_log_id=$1", id)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method)
		VALUES ($1, 'index_drop', '{}', 'rule')`, id); err != nil {
		t.Fatalf("pending row rejected: %v", err)
	}
}

// Re-running the complete migration on a migrated database waits for no
// lock on sage.action_log, the busiest pg_sage table.
func TestActionOutcomeMigration_RerunTakesNoLockOnActionLog(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE sage.action_log IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock sage.action_log: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET lock_timeout = '1s'"); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET lock_timeout") }()
	start := time.Now()
	if _, err := conn.Exec(ctx, ddlActionOutcome()); err != nil {
		t.Fatalf("re-running the migration waited for a lock: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("re-run took %v, want no lock wait", elapsed)
	}
}
