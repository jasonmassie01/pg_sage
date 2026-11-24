package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-05: a VACUUM proposal whose target is already being vacuumed is
// withheld as vacuum_in_progress, writes no failed action row and so never
// counts toward the custodian backoff.

// startThrottledVacuum runs a slow VACUUM of table on its own session and
// returns that backend's pid once the progress view shows that backend.
func startThrottledVacuum(t *testing.T, f *vacuumFixture) int {
	t.Helper()
	conn, err := f.pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire vacuum session: %v", err)
	}
	pid := int(conn.Conn().PgConn().PID())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.Background()
		_, _ = conn.Exec(ctx, "SET vacuum_cost_delay = '50ms'")
		_, _ = conn.Exec(ctx, "SET vacuum_cost_limit = 1")
		_, _ = conn.Exec(ctx, "VACUUM (FREEZE) "+f.table)
		_, _ = conn.Exec(ctx, "RESET vacuum_cost_delay")
		_, _ = conn.Exec(ctx, "RESET vacuum_cost_limit")
		conn.Release()
	}()
	t.Cleanup(func() {
		terminateVacuumsOn(f.pool, f.table)
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var running bool
		if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1
			FROM pg_stat_progress_vacuum WHERE pid = $1 AND relid = to_regclass($2))`,
			pid, f.table).Scan(&running); err != nil {
			t.Fatalf("read progress of pid %d: %v", pid, err)
		}
		if running {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the throttled VACUUM never appeared in pg_stat_progress_vacuum")
	return 0
}

func admissionExecutor(t *testing.T, f *vacuumFixture) (*Executor, int64) {
	t.Helper()
	decisionID := recordCustodianDecision(t, f.ctx, f.pool, "freeze", f.table[len("public."):])
	cfg := config.DefaultConfig()
	exec := New(f.pool, cfg, zeroTime(), func(c, format string, a ...any) {
		t.Logf(c+": "+format, a...)
	})
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe, DecisionID: decisionID,
	}})
	return exec, decisionID
}

func redVacuumProposal(table, sql string) CustodianProposal {
	return CustodianProposal{Feature: "freeze", SQL: sql, ResponseKind: "vacuum_freeze",
		TargetObjects: []string{table},
		Deadline: &policy.DeadlineContext{Kind: policy.DeadlineXID,
			Urgency: policy.UrgencyCritical, HardAt: time.Now().Add(time.Hour)},
		Evidence: map[string]any{}, ObservedAt: time.Now()}
}

// CHECK-02: a running vacuum on the target yields vacuum_in_progress and
// zero failed rows; the backoff count stays 0.
func TestCustodianVacuumWithheldWhileTargetIsBeingVacuumed(t *testing.T) {
	f := newVacuumFixture(t, 40000, "0")
	pid := startThrottledVacuum(t, f)
	exec, decisionID := admissionExecutor(t, f)
	sql := "VACUUM " + f.table
	sub, err := exec.SubmitCustodianProposalDecision(f.ctx, redVacuumProposal(f.table, sql))
	if !errors.Is(err, ErrCustodianProposalWithheld) {
		t.Fatalf("submit error = %v, want ErrCustodianProposalWithheld", err)
	}
	if sub.Executed || sub.Decision.BlockedReason != reasonVacuumInProgress {
		t.Fatalf("submission = %+v, want withheld as vacuum_in_progress", sub)
	}
	var failed, rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE outcome IN
		('failed','rollback_failed')), count(*) FROM sage.action_log WHERE sql_executed = $1`,
		sql).Scan(&failed, &rows); err != nil {
		t.Fatalf("read action rows: %v", err)
	}
	if failed != 0 || rows != 0 {
		t.Fatalf("action_log rows = %d (failed %d), want none", rows, failed)
	}
	failures, _, err := exec.readCustodianFailures(f.ctx, sql)
	if err != nil || failures != 0 {
		t.Fatalf("backoff failures = %d (%v), want 0", failures, err)
	}
	var verdict, reason string
	var gotPID int
	var phase string
	if err := f.pool.QueryRow(f.ctx, `SELECT verdict, reason,
		(evidence->'vacuum_progress'->>'pid')::int, evidence->'vacuum_progress'->>'phase'
		FROM sage.decision WHERE id = $1`, decisionID).
		Scan(&verdict, &reason, &gotPID, &phase); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if verdict != "blocked" || reason != reasonVacuumInProgress || gotPID != pid ||
		phase == "" {
		t.Fatalf("decision = %s/%s pid %d phase %q, want blocked vacuum_in_progress pid %d",
			verdict, reason, gotPID, phase, pid)
	}
}

// With nothing vacuuming the target, the same proposal runs.
func TestCustodianVacuumRunsWhenNothingVacuumsTheTarget(t *testing.T) {
	f := newFreezeFixture(t, 2000, "0")
	exec, decisionID := admissionExecutor(t, f)
	// FREEZE: the post-check needs the horizon to advance, which a plain
	// VACUUM of a table younger than its freeze table age need not do.
	sql := "VACUUM (FREEZE) " + f.table
	sub, err := exec.SubmitCustodianProposalDecision(f.ctx, redVacuumProposal(f.table, sql))
	if err != nil || !sub.Executed {
		t.Fatalf("submit = %+v, %v; want executed", sub, err)
	}
	var progress *string
	if err := f.pool.QueryRow(f.ctx, `SELECT evidence->>'vacuum_progress'
		FROM sage.decision WHERE id = $1`, decisionID).Scan(&progress); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if progress != nil {
		t.Fatalf("an empty progress view recorded %q, want nothing", *progress)
	}
}

// Another table's vacuum is not this table's.
func TestReadRunningVacuumsIgnoresOtherTables(t *testing.T) {
	f := newVacuumFixture(t, 40000, "0")
	// One fixture per test: requireDB holds the cross-package test lock.
	other := f.table + "_other"
	if _, err := f.pool.Exec(f.ctx, "CREATE TABLE "+other+" (a int)"); err != nil {
		t.Fatalf("create %s: %v", other, err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+other) })
	startThrottledVacuum(t, f)
	rows, err := readRunningVacuums(f.ctx, f.pool, other)
	if err != nil || len(rows) != 0 {
		t.Fatalf("other table's running vacuums = %+v, %v; want none", rows, err)
	}
	rows, err = readRunningVacuums(f.ctx, f.pool, f.table)
	if err != nil || len(rows) != 1 || !rows[0].RelIDKnown || rows[0].Autovacuum ||
		rows[0].HeapBlksTotal <= 0 {
		t.Fatalf("target's running vacuums = %+v, %v; want one manual vacuum", rows, err)
	}
}
