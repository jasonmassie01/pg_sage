package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// Retention delete through Executor.Apply (debt item 1), end to end: the
// D5 enforcer's batch runs inside the executor's single pipeline, keeps its
// contract binding, never deletes more than the reviewed dry run's bound,
// verifies the rows it deleted before committing, and is recorded as an
// action with a verification.

// executorPipeline mirrors the sidecar's runtime adapter.
func executorPipeline(exec *executor.Executor) RetentionPipeline {
	return func(ctx context.Context, intent RetentionIntent, batch RetentionBatch) error {
		_, err := exec.ExecuteRetention(ctx, executor.RetentionRequest{
			Target: intent.Schema + "." + intent.Table, Column: intent.Column,
			DeclaredColumn: intent.DeclaredColumn, Cutoff: intent.Cutoff,
			Window: intent.Window, BatchLimit: intent.BatchLimit,
			Candidates: intent.Candidates, Bound: intent.Bound, DryRunID: intent.DryRunID,
			Delete: func(ctx context.Context, run executor.RetentionExecution,
			) (executor.RetentionOutcome, error) {
				result, err := batch(ctx, RetentionRun{ActionID: run.ActionID,
					LockTimeout: time.Duration(run.LockTimeoutMS) * time.Millisecond,
					MaxRows:     run.MaxRows})
				return executor.RetentionOutcome{RunID: result.RunID, Deleted: result.Deleted,
					OutsidePredicate: result.OutsidePredicate,
					OutsideRelation:  result.OutsideRelation}, err
			},
		})
		return err
	}
}

func pipelineExecutor(pool *pgxpool.Pool) *executor.Executor {
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.MaintenanceWindow = "autonomous", "always"
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	exec := executor.New(pool, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	exec.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	// The shared test database accumulates actions across runs; usage
	// limits are not what these tests are about.
	doc.BlastRadius.MaxTablesPerWindow = 1 << 30
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 1 << 30
	exec.EnableStandingPolicyDocument(doc, nil)
	return exec
}

func pipelineEnforcer(pool *pgxpool.Pool, exec *executor.Executor) *postgresRetentionEnforcer {
	return &postgresRetentionEnforcer{pool: pool, batchLimit: 10,
		pipeline: executorPipeline(exec)}
}

func applyThroughPipeline(t *testing.T, pool *pgxpool.Pool, enforcer *postgresRetentionEnforcer,
	table string,
) error {
	t.Helper()
	agedDryRun(t, pool, enforcer, retentionItem(table, retentionTestWindow,
		schemaguard.DispositionDryRun))
	return enforcer.Apply(context.Background(), retentionItem(table, retentionTestWindow,
		schemaguard.DispositionApply))
}

func TestRetentionDeletesThroughExecutorPipeline(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	enforcer := pipelineEnforcer(pool, pipelineExecutor(pool))

	err := applyThroughPipeline(t, pool, enforcer, table)

	if err != nil {
		t.Fatalf("apply through the pipeline: %v", err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 1 {
		t.Fatalf("rows = %d, want only the row inside the retention window", n)
	}
	var deleted, dryRunID, reviewedID int64
	var actionType, outcome, verdict string
	err = pool.QueryRow(context.Background(), `SELECT rr.deleted_rows, rr.dry_run_id,
		al.action_type, al.outcome, v.verdict
		FROM sage.retention_run rr
		JOIN sage.action_log al ON al.id = rr.action_id
		JOIN sage.verification v ON v.action_log_id = al.id
		WHERE rr.table_name=$1 AND rr.disposition='applied'`, table).
		Scan(&deleted, &dryRunID, &actionType, &outcome, &verdict)
	if err != nil {
		t.Fatalf("read applied run, its action and verification: %v", err)
	}
	reviewedID = countRows(t, pool, fmt.Sprintf(`SELECT id FROM sage.retention_run
		WHERE table_name='%s' AND disposition='dry_run'`, table))
	if deleted != 2 || dryRunID != reviewedID || actionType != "retention_delete" ||
		outcome != "success" || verdict != "success" {
		t.Fatalf("applied run deleted=%d dry_run=%d (want %d) action=%s/%s verdict=%s",
			deleted, dryRunID, reviewedID, actionType, outcome, verdict)
	}
}

// redeclareGate re-declares the table's contract right after the first
// authorization, as an owner would while the batch waits for its lease.
type redeclareGate struct {
	inner policy.Gate
	once  func()
	calls int
}

func (g *redeclareGate) Authorize(ctx context.Context, req policy.ActionRequest) policy.Decision {
	g.calls++
	decision := g.inner.Authorize(ctx, req)
	if g.calls == 1 {
		g.once()
	}
	return decision
}

func TestRetentionContractChangedBeforeExecutionRefused(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	exec := pipelineExecutor(pool)
	exec.WithPolicyGate(&redeclareGate{inner: exec.StandingPolicyGate(), once: func() {
		execAll(t, pool, fmt.Sprintf(`UPDATE sage.table_contract
			SET updated_at = now() + interval '1 second' WHERE table_name='%s'`, table))
	}})

	err := applyThroughPipeline(t, pool, pipelineEnforcer(pool, exec), table)

	if err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("apply after a re-declaration = %v, want the identity recheck to refuse",
			err)
	}
	requireNothingDeleted(t, pool, table, 3)
	outcome := ""
	if err := pool.QueryRow(context.Background(), `SELECT outcome FROM sage.action_log
		WHERE action_type='retention_delete' AND sql_executed LIKE $1`, "%"+table+"%").
		Scan(&outcome); err != nil {
		t.Fatalf("read refused action: %v", err)
	}
	if outcome != "failed" {
		t.Fatalf("refused batch outcome = %q, want failed", outcome)
	}
}

func setEmergencyStop(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := executor.SetEmergencyStop(context.Background(), pool, true, "test"); err != nil {
		t.Fatalf("engage emergency stop: %v", err)
	}
	t.Cleanup(func() {
		if err := executor.SetEmergencyStop(context.Background(), pool, false,
			"test"); err != nil {
			t.Errorf("release emergency stop: %v", err)
		}
	})
}

// The emergency stop is pressed while the batch waits: the
// re-authorization refuses and nothing is deleted or recorded as applied.
func TestRetentionStopDuringWaitDeletesNothing(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	exec := pipelineExecutor(pool)
	gate := &redeclareGate{inner: exec.StandingPolicyGate(),
		once: func() { setEmergencyStop(t, pool) }}
	exec.WithPolicyGate(gate)

	err := applyThroughPipeline(t, pool, pipelineEnforcer(pool, exec), table)

	if !errors.Is(err, executor.ErrCustodianProposalWithheld) ||
		!strings.Contains(err.Error(), "emergency_stop") || gate.calls != 2 {
		t.Fatalf("apply with a stop during the wait = %v after %d authorizations, "+
			"want withheld by the re-authorization", err, gate.calls)
	}
	requireNothingDeleted(t, pool, table, 3)
}

// The reviewed dry run authorizes at most its drift ceiling in total: once
// applied batches used it up, deletion parks until a new dry run passes
// review.
func TestRetentionBoundExhaustedNeedsNewDryRun(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10,
		pipeline: allowRetention}
	agedDryRun(t, pool, enforcer, retentionItem(table, retentionTestWindow,
		schemaguard.DispositionDryRun))
	ceiling := retentionDriftFactor*2 + retentionDriftSlack
	execAll(t, pool, fmt.Sprintf(`INSERT INTO sage.retention_run
		(schema_name, table_name, retention_column, cutoff_at, candidate_rows,
		 deleted_rows, disposition, dry_run_id)
		SELECT schema_name, table_name, retention_column, now(), 2, %d, 'applied', id
		FROM sage.retention_run WHERE table_name='%s' AND disposition='dry_run'`,
		ceiling, table))

	err := enforcer.Apply(context.Background(), retentionItem(table, retentionTestWindow,
		schemaguard.DispositionApply))

	var parked *schemaguard.ParkedRoute
	if !errors.Is(err, ErrRetentionDryRunPending) || !errors.As(err, &parked) {
		t.Fatalf("apply past the reviewed bound = %v, want a parked pending dry run", err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 3 {
		t.Fatalf("rows = %d, want nothing deleted past the reviewed bound", n)
	}
	fresh := countRows(t, pool, fmt.Sprintf(`SELECT count(*) FROM sage.retention_run
		WHERE table_name='%s' AND disposition='dry_run'
		AND created_at > now() - interval '1 hour'`, table))
	if fresh != 1 {
		t.Fatalf("fresh dry runs = %d, want 1 to restart review", fresh)
	}
}

// The intent carries what the reviewed dry run still authorizes.
func TestRetentionIntentCarriesReviewedBound(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	var seen RetentionIntent
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10,
		pipeline: func(ctx context.Context, intent RetentionIntent, batch RetentionBatch) error {
			seen = intent
			_, err := batch(ctx, RetentionRun{})
			return err
		}}

	if err := applyThroughPipeline(t, pool, enforcer, table); err != nil {
		t.Fatalf("apply: %v", err)
	}

	reviewed := countRows(t, pool, fmt.Sprintf(`SELECT id FROM sage.retention_run
		WHERE table_name='%s' AND disposition='dry_run'`, table))
	if seen.DryRunID != reviewed || seen.Bound != retentionDriftFactor*2+retentionDriftSlack {
		t.Fatalf("intent dry run=%d bound=%d, want dry run %d and the reviewed ceiling %d",
			seen.DryRunID, seen.Bound, reviewed, retentionDriftFactor*2+retentionDriftSlack)
	}
}

func TestVerifyRetentionBatch(t *testing.T) {
	tests := []struct {
		name    string
		result  RetentionResult
		maxRows int64
		ok      bool
	}{
		{"within bound", RetentionResult{Deleted: 3}, 3, true},
		{"nothing to delete", RetentionResult{}, 5, true},
		{"over the row cap", RetentionResult{Deleted: 4}, 3, false},
		{"row outside the declared predicate",
			RetentionResult{Deleted: 2, OutsidePredicate: 1}, 5, false},
		{"row from another relation", RetentionResult{Deleted: 2, OutsideRelation: 1}, 5, false},
		{"negative count", RetentionResult{Deleted: -1}, 5, false},
		{"no cap", RetentionResult{Deleted: 1}, 0, false},
	}
	for _, tt := range tests {
		err := verifyRetentionBatch(tt.result, tt.maxRows)
		if tt.ok != (err == nil) {
			t.Errorf("%s: verifyRetentionBatch = %v, want ok=%v", tt.name, err, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrRetentionVerification) {
			t.Errorf("%s: error %v does not wrap ErrRetentionVerification", tt.name, err)
		}
	}
}

func TestRetentionBatchRecordsActionAndDryRun(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10}
	item := retentionItem(table, retentionTestWindow, schemaguard.DispositionApply)
	plan := retentionPlan{item: item, target: mustRetentionTarget(t, pool, item.Invariant),
		cutoff: time.Now().Add(-retentionTestWindow), candidates: 2, dryRunID: 31, bound: 10}

	result, err := enforcer.deleteBatch(context.Background(), plan,
		RetentionRun{ActionID: 4242, MaxRows: 1})

	if err != nil || result.Deleted != 1 || result.RunID <= 0 {
		t.Fatalf("deleteBatch = %+v, %v; want one row (the pipeline's cap)", result, err)
	}
	var actionID, dryRunID, deleted int64
	if err := pool.QueryRow(context.Background(), `SELECT action_id, dry_run_id,
		deleted_rows FROM sage.retention_run WHERE id=$1`, result.RunID).
		Scan(&actionID, &dryRunID, &deleted); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if actionID != 4242 || dryRunID != 31 || deleted != 1 {
		t.Fatalf("run action=%d dry_run=%d deleted=%d, want 4242/31/1", actionID, dryRunID,
			deleted)
	}
}

func TestRetentionBatchUsesPipelineLockTimeout(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10}
	item := retentionItem(table, retentionTestWindow, schemaguard.DispositionApply)
	plan := retentionPlan{item: item, target: mustRetentionTarget(t, pool, item.Invariant),
		cutoff: time.Now().Add(-retentionTestWindow), bound: 10}
	started := time.Now()

	_, err = enforcer.deleteBatch(ctx, plan, RetentionRun{LockTimeout: 100 * time.Millisecond})

	if err == nil || time.Since(started) > 1500*time.Millisecond {
		t.Fatalf("deleteBatch under lock = %v after %s, want the 100ms pipeline lock timeout",
			err, time.Since(started))
	}
}

func TestRetentionBatchRefusesWithoutBound(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10}
	item := retentionItem(table, retentionTestWindow, schemaguard.DispositionApply)
	plan := retentionPlan{item: item, target: mustRetentionTarget(t, pool, item.Invariant),
		cutoff: time.Now().Add(-retentionTestWindow)}

	_, err := enforcer.deleteBatch(context.Background(), plan, RetentionRun{})

	if err == nil {
		t.Fatal("a batch without a reviewed bound ran")
	}
	requireNothingDeleted(t, pool, table, 3)
}

// The batch enforces the reviewed bound itself, even when the pipeline
// passes no row cap.
func TestRetentionBatchStopsAtReviewedBound(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10}
	item := retentionItem(table, retentionTestWindow, schemaguard.DispositionApply)
	plan := retentionPlan{item: item, target: mustRetentionTarget(t, pool, item.Invariant),
		cutoff: time.Now().Add(-retentionTestWindow), candidates: 2, bound: 1}

	result, err := enforcer.deleteBatch(context.Background(), plan, RetentionRun{})

	if err != nil || result.Deleted != 1 {
		t.Fatalf("deleteBatch = %+v, %v; want exactly the one row the bound allows",
			result, err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 2 {
		t.Fatalf("rows = %d, want 2 left (one eligible row kept by the bound)", n)
	}
}
