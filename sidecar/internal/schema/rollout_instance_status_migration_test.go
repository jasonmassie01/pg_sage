package schema

import (
	"context"
	"strings"
	"testing"
)

// executor-04: the canary's instance records take verification_pending
// and applied_unverified; any other status is still refused, the widening
// runs twice cleanly, and an install with the old constraint is widened.

func insertRolloutInstance(ctx context.Context, t *testing.T, run, status string) error {
	t.Helper()
	pool, _ := requireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.rollout_instance (run_evidence_id,
		instance_id, ordinal, status) VALUES ($1, $2, 1, $3)`, run, "db_"+status, status)
	return err
}

func rolloutRunFixture(ctx context.Context, t *testing.T) string {
	t.Helper()
	pool, _ := requireDB(t)
	const run = "canary-executor04-migration"
	clean := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.rollout_instance WHERE run_evidence_id = $1", run)
		_, _ = pool.Exec(bg, "DELETE FROM sage.rollout_run WHERE evidence_id = $1", run)
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.rollout_run (evidence_id,
		source_instance, prior_evidence_id, policy, state)
		VALUES ($1, 'src', 'action_log:src:1', '{}', 'canary')`, run); err != nil {
		t.Fatalf("insert rollout run: %v", err)
	}
	return run
}

func TestRolloutInstanceStatusMigrationTwiceAndChecked(t *testing.T) {
	pool, ctx := requireDB(t)
	for i := 0; i < 2; i++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	run := rolloutRunFixture(ctx, t)
	for _, status := range []string{"verification_pending", "applied_unverified", "applied",
		"rolled_back", "failed"} {
		if err := insertRolloutInstance(ctx, t, run, status); err != nil {
			t.Errorf("status %s refused: %v", status, err)
		}
	}
	err := insertRolloutInstance(ctx, t, run, "judged_later")
	if err == nil || !strings.Contains(err.Error(), "rollout_instance_status_check") {
		t.Fatalf("an unknown status was accepted or refused oddly: %v", err)
	}
	var checks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.rollout_instance'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%status%'`).Scan(&checks); err != nil ||
		checks != 1 {
		t.Fatalf("status checks = %d (%v), want exactly 1", checks, err)
	}
}

// State transition: an install whose constraint predates executor-04 is
// widened by the next bootstrap.
func TestRolloutInstanceStatusMigrationWidensTheOldConstraint(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.rollout_instance
		DROP CONSTRAINT rollout_instance_status_check;
		ALTER TABLE sage.rollout_instance ADD CONSTRAINT rollout_instance_status_check
		CHECK (status IN ('not_locally_verified', 'applied', 'rolled_back',
		'rollback_failed', 'rollback_unavailable', 'failed'))`); err != nil {
		t.Fatalf("restore the old constraint: %v", err)
	}
	run := rolloutRunFixture(ctx, t)
	if err := insertRolloutInstance(ctx, t, run, "verification_pending"); err == nil {
		t.Fatal("the old constraint accepted verification_pending")
	}
	bootstrapWithRetry(t, ctx, pool)
	if err := insertRolloutInstance(ctx, t, run, "verification_pending"); err != nil {
		t.Fatalf("after bootstrap, verification_pending refused: %v", err)
	}
}
