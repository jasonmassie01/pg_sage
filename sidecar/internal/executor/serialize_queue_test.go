package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// serialize_mode queue (debt item 3) through Apply: a self-initiated
// action whose target is leased waits its FIFO turn and then runs, instead
// of parking; a wait past the queue bound parks it exactly like park mode.

func TestStandingDecisionCarriesSerializeMode(t *testing.T) {
	got := standingPolicyDecision(policy.Decision{
		Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized,
		SerializeMode: policy.SerializeQueue,
	})
	if got.SerializeMode != policy.SerializeQueue {
		t.Fatalf("serialize mode = %q, want queue", got.SerializeMode)
	}
}

// queueExecutor takes the test's pool: requireDB holds a cross-package
// lock per call, so a second call in one test would wait on itself.
func queueExecutor(pool *pgxpool.Pool, maxWait time.Duration) *Executor {
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	cfg := policy.DefaultLeaseQueueConfig()
	cfg.PollInterval, cfg.MaxWait = 20*time.Millisecond, maxWait
	exec.WithLeaseQueue(cfg)
	return exec
}

func queueDecision(id int64) ActionPolicyDecision {
	return ActionPolicyDecision{DecisionID: id, SerializeMode: policy.SerializeQueue}
}

// Two writers race for one table in queue mode: the second waits for the
// first, then runs. Neither parks and neither fails.
func TestQueueModeSecondWriterRunsAfterFirst(t *testing.T) {
	pool, ctx := requireDB(t)
	setupParkTable(t, ctx, pool)
	exec := queueExecutor(pool, 20*time.Second)
	first, second := insertParkDecision(t, ctx, pool), insertParkDecision(t, ctx, pool)
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(ctx,
		`LOCK TABLE public.`+parkTable+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		exec.executeFinding(ctx, parkFinding(), 0, queueDecision(first))
	}()
	waitForActiveLease(t, ctx, pool, first)
	go func() {
		defer wg.Done()
		exec.executeFinding(ctx, parkFinding(), 0, queueDecision(second))
	}()
	waitForQueuedEntry(t, pool, parkFinding().RecommendedSQL)

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	wg.Wait()

	for _, decision := range []int64{first, second} {
		if got := parkDecisions(t, ctx, pool, decision); got != 0 {
			t.Fatalf("decision %d parked %d times in queue mode, want 0", decision, got)
		}
		rows := actionRows(t, ctx, pool, decision)
		if len(rows) != 1 || rows[0] == "failed" {
			t.Fatalf("decision %d action rows = %v, want one non-failed row", decision, rows)
		}
	}
}

// A queue wait past the bound parks a self-initiated action: no failed
// row (no retry/abandon count, no rate budget), one park decision that
// names the queue timeout.
func TestQueueModeTimeoutParksSelfInitiated(t *testing.T) {
	pool, ctx := requireDB(t)
	setupParkTable(t, ctx, pool)
	exec := queueExecutor(pool, 300*time.Millisecond)
	holdTypedLease(t, pool, "custodian", "public."+parkTable)
	decision := insertParkDecision(t, ctx, pool)
	started := time.Now()

	exec.executeFinding(ctx, parkFinding(), 0, queueDecision(decision))

	if elapsed := time.Since(started); elapsed < 250*time.Millisecond ||
		elapsed > 10*time.Second {
		t.Fatalf("queued for %s, want about the 300ms queue bound", elapsed)
	}
	if rows := actionRows(t, ctx, pool, decision); len(rows) != 0 {
		t.Fatalf("timed-out queue wait wrote action rows %v, want none", rows)
	}
	if got := parkDecisions(t, ctx, pool, decision); got != 1 {
		t.Fatalf("park decisions = %d, want 1", got)
	}
	var queueOutcome string
	if err := pool.QueryRow(ctx, `SELECT evidence->>'lease_queue' FROM sage.decision
		WHERE verdict='parked' AND (evidence->>'parked_decision_id')::bigint=$1`,
		decision).Scan(&queueOutcome); err != nil {
		t.Fatalf("read park evidence: %v", err)
	}
	if queueOutcome != "timeout" {
		t.Fatalf("park evidence lease_queue = %q, want timeout", queueOutcome)
	}
}

// Park mode is unchanged by the queue: a conflict parks at once and
// creates no queue entry.
func TestParkModeDoesNotQueue(t *testing.T) {
	pool, ctx := requireDB(t)
	setupParkTable(t, ctx, pool)
	exec := queueExecutor(pool, 20*time.Second)
	holdTypedLease(t, pool, "custodian", "public."+parkTable)
	decision := insertParkDecision(t, ctx, pool)
	started := time.Now()

	exec.executeFinding(ctx, parkFinding(), 0, ActionPolicyDecision{
		DecisionID: decision, SerializeMode: policy.SerializePark})

	if time.Since(started) > 5*time.Second {
		t.Fatalf("park mode waited %s, want an immediate park", time.Since(started))
	}
	if got := parkDecisions(t, ctx, pool, decision); got != 1 {
		t.Fatalf("park decisions = %d, want 1", got)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.lease_queue WHERE intent=$1
		AND decision_id=$2`, parkFinding().RecommendedSQL, decision).Scan(&queued); err != nil {
		t.Fatalf("count queue entries: %v", err)
	}
	if queued != 0 {
		t.Fatalf("park mode created %d queue entries, want 0", queued)
	}
}
