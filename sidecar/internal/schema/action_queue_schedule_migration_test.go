package schema

import (
	"strings"
	"testing"
)

// policy-04: the schedule columns and the due-row index of the action
// queue. The migration is idempotent and runs under lock_timeout.

func TestActionQueueScheduleMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, col := range []string{"scheduled_by", "scheduled_at", "run_after",
		"window_closes_at"} {
		var nullable string
		if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'sage' AND table_name = 'action_queue'
			  AND column_name = $1`, col).Scan(&nullable); err != nil || nullable != "YES" {
			t.Errorf("sage.action_queue.%s missing or not nullable (%q, %v)", col, nullable,
				err)
		}
	}
	var predicate string
	if err := pool.QueryRow(ctx, `SELECT pg_get_expr(i.indpred, i.indrelid)
		FROM pg_index i WHERE i.indexrelid = 'sage.idx_action_queue_scheduled_due'::regclass`,
	).Scan(&predicate); err != nil || !strings.Contains(predicate, "scheduled") {
		t.Fatalf("partial index predicate = %q (%v), want status = 'scheduled'", predicate, err)
	}
}

func TestActionQueueScheduleMigrationIsGuarded(t *testing.T) {
	// No database: the statement text itself is idempotent and bounded.
	for _, want := range []string{"SET LOCAL lock_timeout", "ADD COLUMN IF NOT EXISTS scheduled_by",
		"ADD COLUMN IF NOT EXISTS run_after", "CREATE INDEX IF NOT EXISTS"} {
		if !strings.Contains(ddlActionQueueSchedule, want) {
			t.Errorf("ddlActionQueueSchedule lacks %q", want)
		}
	}
}
