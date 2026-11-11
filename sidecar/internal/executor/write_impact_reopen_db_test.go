package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-03 on real PostgreSQL: a v2.5.0 write_impact revert whose drop
// was withheld (an emergency stop or standing policy left the action
// rollback_skipped, the watch open) is reopened by the startup sweep. When
// its queries prove the index, it must settle as kept: the action is a
// success, not a skipped rollback that the hysteresis and the cards read
// as a regression.
func TestReopenedWriteImpactRevertSettlesAsKept(t *testing.T) {
	for _, prior := range []string{"rollback_skipped", "rollback_failed"} {
		t.Run(prior, func(t *testing.T) {
			f, ctx := newCreateFixture(t)
			pool := f.exec.pool
			seedQuerySamples(t, ctx, pool, f.qid, f.executedAt.Add(-2*time.Hour), 119, 10, 20)
			seedContinuation(t, ctx, pool, f.qid, f.executedAt, 119, 10, 8)
			createWriteImpactRevert(t, f, prior)

			if err := f.exec.reopenObservationRetries(ctx); err != nil {
				t.Fatalf("reopen sweep: %v", err)
			}
			// Earlier tests in this package leave open watches with no
			// action (production never writes one); ListDue cannot scan
			// them, so they are moved out of this resume.
			if _, err := pool.Exec(ctx, `UPDATE sage.verification
				SET next_evaluation_at=now()+interval '1 day'
				WHERE completed_at IS NULL AND action_log_id IS NULL`); err != nil {
				t.Fatalf("defer actionless watches: %v", err)
			}
			if err := f.exec.indexVerification.ResumeDue(ctx); err != nil {
				t.Logf("ResumeDue reported other watches: %v", err)
			}

			var present int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index
				WHERE indexrelid=to_regclass($1)`, f.index).Scan(&present); err != nil {
				t.Fatalf("read pg_index: %v", err)
			}
			if present != 1 {
				t.Fatalf("index %s present = %d, want kept", f.index, present)
			}
			if v := verificationVerdict(t, pool, f.id); v != "success" {
				t.Fatalf("verification verdict = %q, want success", v)
			}
			if outcome, _ := actionOutcomeFor(t, pool, f.id); outcome != "success" {
				t.Fatalf("action outcome = %q, want success (was %s)", outcome, prior)
			}
			var ledger string
			if err := pool.QueryRow(ctx, `SELECT verdict FROM sage.action_outcome
				WHERE action_log_id=$1`, f.id).Scan(&ledger); err != nil || ledger != "improved" {
				t.Fatalf("outcome ledger = %q, %v; want improved", ledger, err)
			}
		})
	}
}

// An operator's rollback is never undone: a reopened write_impact watch
// leaves an action the operator rolled back as rolled_back.
func TestReopenSweepLeavesOperatorRollbackAlone(t *testing.T) {
	f, ctx := newCreateFixture(t)
	createWriteImpactRevert(t, f, "rolled_back")
	if err := f.exec.reopenObservationRetries(ctx); err != nil {
		t.Fatalf("reopen sweep: %v", err)
	}
	if outcome, _ := actionOutcomeFor(t, f.exec.pool, f.id); outcome != "rolled_back" {
		t.Fatalf("action outcome = %q, want rolled_back kept", outcome)
	}
	if v := verificationVerdict(t, f.exec.pool, f.id); v != "extended" {
		t.Fatalf("verification verdict = %q, want extended (reopened)", v)
	}
}

// createWriteImpactRevert stores the open write_impact revert v2.5.0 left
// behind, its regressed ledger row, and the action outcome its withheld or
// failed drop recorded.
func createWriteImpactRevert(t *testing.T, f createFixture, outcome string) {
	t.Helper()
	ctx, pool := t.Context(), f.exec.pool
	state := verify.WatchState{
		ID: fmt.Sprintf("index-action-%d", f.id), ActionID: f.id, ExecutedAt: f.executedAt,
		Table: "public." + f.table, IndexName: f.index,
		Criterion: verify.Criterion{Kind: "per_query_latency", TargetIDs: []int64{f.qid},
			MinGainPct: 20, RegressPct: 15, WriteImpactPct: 20,
			Window: 2 * time.Hour, HardMax: 72 * time.Hour},
		Status: "pending", Window: 2 * time.Hour,
		NextEvaluationAt: f.executedAt.Add(2 * time.Hour),
	}
	if err := verify.NewPostgresStateStore(pool, 30).Create(ctx, state); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	// v2.5.0 stored no write_scope: its write_impact came from the
	// database-wide signal (executor-03b marks every newer watch).
	if _, err := pool.Exec(ctx, `UPDATE sage.verification SET verdict='revert',
		reason='write_impact', next_evaluation_at=now()-interval '1 minute',
		baseline = baseline - 'write_scope'
		WHERE action_log_id=$1`, f.id); err != nil {
		t.Fatalf("simulate the v2.5.0 revert: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET outcome=$2,
		rollback_reason='emergency stop active; automatic rollback withheld'
		WHERE id=$1`, f.id, outcome); err != nil {
		t.Fatalf("simulate the withheld drop: %v", err)
	}
	err := verify.NewOutcomeStore(pool).RecordVerdict(ctx, verify.Outcome{
		ActionLogID: f.id, Class: verify.ClassIndexCreate, Verdict: verify.OutcomeRegressed,
		Reason: "write_impact", Observed: verify.Observed{Metric: verify.MetricMeanExecTime},
	})
	if err != nil {
		t.Fatalf("record the v2.5.0 regressed outcome: %v", err)
	}
}
