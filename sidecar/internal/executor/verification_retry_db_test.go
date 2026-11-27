package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-02 on real PostgreSQL: a watch that a transient read error closed
// before the fix (unverifiable, observation_unavailable, hard deadline
// ahead) used to replay its revert on every resume and drop the index. The
// startup sweep reopens it, and the next resume observes it and keeps the
// index the evidence supports.
func TestStartupSweepObservesPreFixRowInsteadOfDroppingIndex(t *testing.T) {
	f, ctx := newCreateFixture(t)
	pool := f.exec.pool
	seedQuerySamples(t, ctx, pool, f.qid, f.executedAt.Add(-2*time.Hour), 119, 10, 20)
	seedContinuation(t, ctx, pool, f.qid, f.executedAt, 119, 10, 8)
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
	if _, err := pool.Exec(ctx, `UPDATE sage.verification SET verdict='unverifiable',
		reason='observation_unavailable', next_evaluation_at=now()-interval '1 minute'
		WHERE action_log_id=$1`, f.id); err != nil {
		t.Fatalf("simulate the pre-fix row: %v", err)
	}

	f.exec.reopenObservationRetries(ctx)
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
		t.Fatalf("verification verdict = %q, want success (observed again)", v)
	}
	if outcome, _ := actionOutcomeFor(t, pool, f.id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success", outcome)
	}
}
