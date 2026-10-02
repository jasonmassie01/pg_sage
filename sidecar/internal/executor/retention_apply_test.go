package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Retention delete through Apply (debt item 1): a D5 retention batch runs
// in the single pipeline (authorize -> typed lease -> slot ->
// re-authorize -> execute -> verify) and is recorded in action_log with a
// verification, instead of running beside it after an authorize-only call.

func retentionTable(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	table := fmt.Sprintf("ret_apply_%d", time.Now().UnixNano())
	if _, err := pool.Exec(context.Background(), "CREATE TABLE public."+table+
		" (id bigint, created_at timestamptz NOT NULL)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS public."+table)
	})
	return table
}

func retentionExecutor(pool *pgxpool.Pool, mode string) *Executor {
	exec := New(pool, wave1PolicyConfig("autonomous"),
		time.Now().Add(-40*24*time.Hour), nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	return withSerializeGate(exec, mode)
}

// retentionStub stands in for the D5 batch: it records an applied
// retention_run row for the pipeline's action and reports an outcome.
type retentionStub struct {
	pool          *pgxpool.Pool
	table         string
	mu            sync.Mutex
	calls         int
	got           RetentionExecution
	leaseHeld     bool
	reported      int64
	recorded      int64
	outside       int64
	foreignAction bool
	err           error
}

func (s *retentionStub) delete(ctx context.Context, run RetentionExecution,
) (RetentionOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.got = run
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.change_lease
		WHERE state='active' AND object_name=$1 AND actor='retention')`,
		"public."+s.table).Scan(&s.leaseHeld); err != nil {
		return RetentionOutcome{}, err
	}
	if s.err != nil {
		return RetentionOutcome{}, s.err
	}
	actionID := run.ActionID
	if s.foreignAction {
		actionID++
	}
	var runID int64
	err := s.pool.QueryRow(ctx, `INSERT INTO sage.retention_run
		(schema_name, table_name, retention_column, cutoff_at, candidate_rows,
		 deleted_rows, disposition, action_id)
		VALUES ('public', $1, 'created_at', now(), 10, $2, 'applied', $3)
		RETURNING id`, s.table, s.recorded, actionID).Scan(&runID)
	return RetentionOutcome{RunID: runID, Deleted: s.reported, OutsidePredicate: s.outside},
		err
}

func retentionRequest(table string, stub *retentionStub) RetentionRequest {
	return RetentionRequest{
		Target: "public." + table, Column: "created_at", DeclaredColumn: "created_at",
		Cutoff: time.Now().Add(-30 * 24 * time.Hour), Window: 30 * 24 * time.Hour,
		BatchLimit: 100, Candidates: 10, Bound: 50, DryRunID: 0, Delete: stub.delete,
	}
}

func retentionActions(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT outcome FROM sage.action_log
		WHERE action_type='retention_delete' AND sql_executed LIKE $1 ORDER BY id`,
		"%"+table+"%")
	if err != nil {
		t.Fatalf("read retention actions: %v", err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			t.Fatalf("scan: %v", err)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func TestExecuteRetentionRunsThroughApply(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializePark)
	stub := &retentionStub{pool: pool, table: table, reported: 7, recorded: 7}

	actionID, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

	if err != nil || actionID <= 0 {
		t.Fatalf("ExecuteRetention = %d, %v; want a recorded action", actionID, err)
	}
	if stub.calls != 1 || stub.got.ActionID != actionID || stub.got.MaxRows != 50 {
		t.Fatalf("batch calls=%d run=%+v, want one run for action %d capped at 50",
			stub.calls, stub.got, actionID)
	}
	if stub.got.LockTimeoutMS <= 0 || stub.got.LockTimeoutMS > 3000 {
		t.Fatalf("batch lock timeout = %dms, want capped by the 3000ms policy ceiling",
			stub.got.LockTimeoutMS)
	}
	if !stub.leaseHeld {
		t.Fatal("the batch ran without the retention lease on its table")
	}
	var actionType, outcome, deleted, verdict string
	var decisionID *int64
	err = pool.QueryRow(ctx, `SELECT al.action_type, al.outcome, al.decision_id,
		al.after_state->>'deleted_rows', v.verdict
		FROM sage.action_log al JOIN sage.verification v ON v.action_log_id = al.id
		WHERE al.id=$1`, actionID).Scan(&actionType, &outcome, &decisionID, &deleted, &verdict)
	if err != nil {
		t.Fatalf("read action and verification: %v", err)
	}
	if actionType != "retention_delete" || outcome != "success" || decisionID == nil ||
		deleted != "7" || verdict != "success" {
		t.Fatalf("action %s/%s decision=%v deleted=%s verdict=%s, want a verified "+
			"retention_delete of 7 rows", actionType, outcome, decisionID, deleted, verdict)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.change_lease
		WHERE state='active' AND object_name=$1`, "public."+table).Scan(&active); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if active != 0 {
		t.Fatalf("active leases after the batch = %d, want released", active)
	}
}

func TestExecuteRetentionEmergencyStopDeletesNothing(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializePark)
	exec.emergencyStopFn = func(context.Context) bool { return true }
	stub := &retentionStub{pool: pool, table: table, reported: 1, recorded: 1}

	_, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

	if !errors.Is(err, ErrCustodianProposalWithheld) ||
		!strings.Contains(err.Error(), "emergency_stop") {
		t.Fatalf("retention under emergency stop = %v, want withheld emergency_stop", err)
	}
	if stub.calls != 0 || len(retentionActions(t, pool, table)) != 0 {
		t.Fatalf("stopped retention ran %d batches / %v actions, want none", stub.calls,
			retentionActions(t, pool, table))
	}
}

// stopAtGate engages the emergency stop after the first authorization, as
// an operator pressing stop while the action waits for its lease and slot.
type stopAtGate struct {
	inner policy.Gate
	stop  func()
	calls int
}

func (g *stopAtGate) Authorize(ctx context.Context, req policy.ActionRequest) policy.Decision {
	g.calls++
	decision := g.inner.Authorize(ctx, req)
	if g.calls == 1 {
		g.stop()
	}
	return decision
}

func TestExecuteRetentionStopDuringWaitDeletesNothing(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializePark)
	var stopped bool
	exec.emergencyStopFn = func(context.Context) bool { return stopped }
	gate := &stopAtGate{inner: exec.StandingPolicyGate(), stop: func() { stopped = true }}
	exec.WithPolicyGate(gate)
	stub := &retentionStub{pool: pool, table: table, reported: 1, recorded: 1}

	_, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

	// ExecuteRetention reports withheld deletes like custodians do; a
	// refusal by the re-authorization names its stage ("after lease").
	if !errors.Is(err, ErrCustodianProposalWithheld) ||
		!strings.Contains(err.Error(), "after lease: emergency_stop") {
		t.Fatalf("stop during the wait = %v, want a re-authorization refusal", err)
	}
	if gate.calls != 2 || stub.calls != 0 || len(retentionActions(t, pool, table)) != 0 {
		t.Fatalf("authorizations=%d batches=%d actions=%v, want 2/0/none", gate.calls,
			stub.calls, retentionActions(t, pool, table))
	}
}

func TestExecuteRetentionLeaseConflictParks(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializePark)
	holdTypedLease(t, pool, "custodian", "public."+table)
	stub := &retentionStub{pool: pool, table: table, reported: 1, recorded: 1}

	_, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

	if !errors.Is(err, policy.ErrLeaseConflict) {
		t.Fatalf("retention on a leased table = %v, want ErrLeaseConflict", err)
	}
	if stub.calls != 0 || len(retentionActions(t, pool, table)) != 0 {
		t.Fatalf("parked retention ran %d batches / %v actions, want none", stub.calls,
			retentionActions(t, pool, table))
	}
	var parked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE verdict='parked' AND reason='ddl_conflict' AND target_objects ? $1`,
		"public."+table).Scan(&parked); err != nil {
		t.Fatalf("count parks: %v", err)
	}
	if parked != 1 {
		t.Fatalf("park decisions = %d, want 1", parked)
	}
}

func TestExecuteRetentionQueueWaitsForLease(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializeQueue)
	cfg := policy.DefaultLeaseQueueConfig()
	cfg.PollInterval, cfg.MaxWait = 20*time.Millisecond, 20*time.Second
	exec.WithLeaseQueue(cfg)
	release := holdTypedLease(t, pool, "custodian", "public."+table)
	stub := &retentionStub{pool: pool, table: table, reported: 2, recorded: 2}
	started := time.Now()
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	actionID, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

	if err != nil || actionID <= 0 {
		t.Fatalf("queued retention = %d, %v; want it to run after the lease frees", actionID,
			err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond || stub.calls != 1 {
		t.Fatalf("batch ran %d times after %s, want once after the holder released",
			stub.calls, elapsed)
	}
}

func TestExecuteRetentionRejectsUnverifiedBatches(t *testing.T) {
	type batchReport struct {
		reported, recorded, outside int64
		foreignAction               bool
	}
	tests := []struct {
		name   string
		report batchReport
	}{
		{"more rows than the reviewed bound", batchReport{reported: 60, recorded: 60}},
		{"rows outside the declared predicate",
			batchReport{reported: 3, recorded: 3, outside: 1}},
		{"reported count differs from the durable record",
			batchReport{reported: 7, recorded: 6}},
		{"durable record belongs to another action",
			batchReport{reported: 2, recorded: 2, foreignAction: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, ctx := requireDB(t)
			table := retentionTable(t, pool)
			exec := retentionExecutor(pool, policy.SerializePark)
			stub := &retentionStub{pool: pool, table: table, reported: tt.report.reported,
				recorded: tt.report.recorded, outside: tt.report.outside,
				foreignAction: tt.report.foreignAction}

			actionID, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

			if !errors.Is(err, ErrRetentionUnverified) {
				t.Fatalf("ExecuteRetention = %v, want ErrRetentionUnverified", err)
			}
			var outcome, verdict string
			if err := pool.QueryRow(ctx, `SELECT al.outcome, v.verdict
				FROM sage.action_log al JOIN sage.verification v ON v.action_log_id=al.id
				WHERE al.id=$1`, actionID).Scan(&outcome, &verdict); err != nil {
				t.Fatalf("read action %d: %v", actionID, err)
			}
			if outcome != "failed" || verdict != "unverifiable" {
				t.Fatalf("action outcome=%s verdict=%s, want failed/unverifiable",
					outcome, verdict)
			}
		})
	}
}

func TestExecuteRetentionBatchErrorRecordsFailure(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializePark)
	batchErr := errors.New("retention column changed identity; nothing deleted")
	stub := &retentionStub{pool: pool, table: table, err: batchErr}

	actionID, err := exec.ExecuteRetention(ctx, retentionRequest(table, stub))

	if !errors.Is(err, batchErr) || actionID <= 0 {
		t.Fatalf("ExecuteRetention = %d, %v; want the batch error on a recorded action",
			actionID, err)
	}
	var outcome, reason string
	if err := pool.QueryRow(ctx, `SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id=$1`, actionID).Scan(&outcome, &reason); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if outcome != "failed" || !strings.Contains(reason, "changed identity") {
		t.Fatalf("action outcome=%s reason=%q, want failed with the batch error",
			outcome, reason)
	}
}

func TestExecuteRetentionRefusesIncompleteRequests(t *testing.T) {
	pool, ctx := requireDB(t)
	table := retentionTable(t, pool)
	exec := retentionExecutor(pool, policy.SerializePark)
	stub := &retentionStub{pool: pool, table: table, reported: 1, recorded: 1}
	for name, mutate := range map[string]func(*RetentionRequest){
		"no batch":       func(r *RetentionRequest) { r.Delete = nil },
		"no bound":       func(r *RetentionRequest) { r.Bound = 0 },
		"negative bound": func(r *RetentionRequest) { r.Bound = -5 },
		"no batch limit": func(r *RetentionRequest) { r.BatchLimit = 0 },
	} {
		request := retentionRequest(table, stub)
		mutate(&request)

		_, err := exec.ExecuteRetention(ctx, request)

		if err == nil || errors.Is(err, ErrActionWithheld) {
			t.Errorf("%s: ExecuteRetention = %v, want an admission error", name, err)
		}
	}
	if stub.calls != 0 || len(retentionActions(t, pool, table)) != 0 {
		t.Fatalf("incomplete requests ran %d batches / %v actions, want none", stub.calls,
			retentionActions(t, pool, table))
	}
	noPool := New(nil, wave1PolicyConfig("autonomous"), zeroTime(), nopLog)
	noPool.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate}})
	if _, err := noPool.ExecuteRetention(ctx, retentionRequest(table, stub)); err == nil ||
		!strings.Contains(err.Error(), "database pool unavailable") {
		t.Fatalf("ExecuteRetention without a database = %v, want pool unavailable", err)
	}
}
