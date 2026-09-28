package recommendation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func expireLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.recommendation
		SET lease_until = now() - interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

// A crash between applying and applied leaves the claim with no durable
// outcome. Once its lease expires, recovery records an interrupted
// attempt as failed (never proposed) and the retry budget still holds.
func TestCrashBetweenApplyingAndApplied(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)

	// Still leased: a live worker owns it; recovery must not touch it.
	n, err := s.RecoverStaleApplying(ctx, rec.DatabaseName)
	if err != nil || n != 0 {
		t.Fatalf("recover under a live lease: n=%d err=%v, want 0", n, err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateApplying {
		t.Fatalf("live claim moved to %s", got.State)
	}

	expireLease(t, ctx, pool, rec.ID)
	n, err = s.RecoverStaleApplying(ctx, rec.DatabaseName)
	if err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v, want 1", n, err)
	}
	got := mustGet(t, ctx, s, rec.ID)
	if got.State != StateFailed || got.AttemptCount != 1 || got.NextAttemptAt == nil ||
		!strings.Contains(got.Reason, "interrupted") || got.ApprovedHash != rec.ContentHash {
		t.Fatalf("recovered head = %+v, want failed/interrupted keeping its approval", got)
	}
	if again, _ := s.RecoverStaleApplying(ctx, rec.DatabaseName); again != 0 {
		t.Fatalf("second recovery touched %d rows, want 0 (idempotent)", again)
	}
	// The dead worker's late write cannot resurrect the claim.
	actionID := insertActionLog(t, ctx, pool, "monitoring", sqlA)
	if err := s.RecordApplied(ctx, c, actionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("late RecordApplied: err=%v, want ErrConflict", err)
	}
	makeDue(t, ctx, pool, rec.ID)
	if c2 := claim(t, ctx, s, mustGet(t, ctx, s, rec.ID)); c2.Attempt != 2 {
		t.Fatalf("resumed attempt = %d, want 2", c2.Attempt)
	}
}

func TestCrashRecoveryExhaustsBudget(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	if _, err := pool.Exec(ctx, `UPDATE sage.recommendation SET retry_budget=0
		WHERE id=$1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	claim(t, ctx, s, rec)
	expireLease(t, ctx, pool, rec.ID)
	if _, err := s.RecoverStaleApplying(ctx, rec.DatabaseName); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateAbandoned {
		t.Fatalf("interrupted last attempt: state=%s, want abandoned", got.State)
	}
}

// The applied transition and the action_log insert share a transaction:
// a crash (rollback) before commit leaves neither.
func TestAppliedAndActionLogAreAtomic(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var actionID int64
	if err := tx.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('create_index', $1, 'monitoring')
		RETURNING id`, sqlA).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if err := RecordAppliedTx(ctx, tx, c, actionID); err != nil {
		t.Fatalf("applied in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var logged int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log WHERE id=$1`,
		actionID).Scan(&logged)
	got := mustGet(t, ctx, s, rec.ID)
	if logged != 0 || got.State != StateApplying || got.ActionLogID != nil {
		t.Fatalf("after rollback: action rows=%d state=%s link=%v, want none/applying",
			logged, got.State, got.ActionLogID)
	}
}

func TestAppliedWithoutVerifyingIsResumed(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)
	actionID := insertActionLog(t, ctx, pool, "monitoring", sqlA)
	if err := s.RecordApplied(ctx, c, actionID); err != nil {
		t.Fatal(err)
	}
	// Crash before StartVerifying: the next reconcile moves it on.
	if _, err := s.ReconcileVerifying(ctx, rec.DatabaseName); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateVerifying {
		t.Fatalf("applied head after reconcile = %s, want verifying", got.State)
	}
}

func verifyingWithOutcome(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Store, outcome string,
) (Recommendation, int64) {
	t.Helper()
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)
	actionID := insertActionLog(t, ctx, pool, "monitoring", sqlA)
	if err := s.RecordApplied(ctx, c, actionID); err != nil {
		t.Fatal(err)
	}
	if err := s.StartVerifying(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET outcome=$2,
		rollback_reason='r' WHERE id=$1`, actionID, outcome); err != nil {
		t.Fatal(err)
	}
	return rec, actionID
}

// C15: the verdict is recorded separately from effect completion. A
// regression whose revert has not happened stays verifying, with the
// verdict visible; only a completed revert reaches reverted.
func TestReconcileVerifyingSeparatesVerdictFromEffect(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	cases := []struct {
		outcome, verdict string
		want             State
	}{
		{"success", "success", StateVerified},
		{"rolled_back", "regressed", StateReverted},
		{"unverifiable", "unverifiable", StateInconclusive},
		{"rollback_failed", "regressed", StateVerifying},
		{"rollback_skipped", "regressed", StateVerifying},
		{"monitoring", "", StateVerifying},
		{"applied_pending_restart", "", StateVerifying},
	}
	for _, tc := range cases {
		rec, _ := verifyingWithOutcome(t, ctx, pool, s, tc.outcome)
		if _, err := s.ReconcileVerifying(ctx, rec.DatabaseName); err != nil {
			t.Fatalf("%s: %v", tc.outcome, err)
		}
		got := mustGet(t, ctx, s, rec.ID)
		if got.State != tc.want || got.Verdict != tc.verdict {
			t.Errorf("outcome %s: state=%s verdict=%q, want %s %q",
				tc.outcome, got.State, got.Verdict, tc.want, tc.verdict)
		}
	}
}

func TestReconcileUsesDurableVerificationVerdict(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec, actionID := verifyingWithOutcome(t, ctx, pool, s, "monitoring")
	var decisionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		verdict, risk_tier, reason, evidence_id) VALUES ('t', 'i', 'execute', 'safe',
		'r', md5(random()::text)) RETURNING id`).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.verification (decision_id,
		action_log_id, criterion, baseline, minimum_samples, next_evaluation_at,
		hard_deadline_at, verdict) VALUES ($1, $2, '{}', '{}', 1, now(), now(), 'revert')`,
		decisionID, actionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileVerifying(ctx, rec.DatabaseName); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, ctx, s, rec.ID)
	if got.State != StateVerifying || got.Verdict != "regressed" {
		t.Fatalf("revert verdict before the revert ran: state=%s verdict=%q, "+
			"want verifying/regressed", got.State, got.Verdict)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET outcome='rolled_back'
		WHERE id=$1`, actionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileVerifying(ctx, rec.DatabaseName); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateReverted {
		t.Fatalf("after the revert completed: state=%s, want reverted", got.State)
	}
}

func TestReconcileIgnoresOtherDatabases(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec, _ := verifyingWithOutcome(t, ctx, pool, s, "success")
	if _, err := s.ReconcileVerifying(ctx, rec.DatabaseName+"_other"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateVerifying {
		t.Fatalf("another database's reconcile moved this row to %s", got.State)
	}
}

func TestSupersedeAbsentLeavesInFlightAlone(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	db := uniqueDB(t)
	p := proposal(t, ctx, pool, db, sqlA, inverseA)
	kept := mustPropose(t, ctx, s, p).Recommendation
	gone := p
	gone.ForwardSQL, gone.InverseSQL = "CREATE INDEX CONCURRENTLY idx_c ON public.orders (c)", ""
	goneRec := mustPropose(t, ctx, s, gone).Recommendation
	busy := p
	busy.ForwardSQL = "CREATE INDEX CONCURRENTLY idx_d ON public.orders (d)"
	busyRec := mustPropose(t, ctx, s, busy).Recommendation
	setState(t, ctx, pool, busyRec.ID, StateApplying)

	n, err := s.SupersedeAbsent(ctx, db, p.Category, map[string]bool{kept.IdentityKey: true})
	if err != nil || n != 1 {
		t.Fatalf("supersede: n=%d err=%v, want 1", n, err)
	}
	if got := mustGet(t, ctx, s, goneRec.ID); got.State != StateSuperseded {
		t.Fatalf("absent candidate = %s, want superseded", got.State)
	}
	if got := mustGet(t, ctx, s, kept.ID); got.State != StateProposed {
		t.Fatalf("present candidate = %s, want proposed", got.State)
	}
	if got := mustGet(t, ctx, s, busyRec.ID); got.State != StateApplying {
		t.Fatalf("in-flight candidate = %s, want applying", got.State)
	}
}

func TestCheckFreshAndListActionable(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	db := uniqueDB(t)
	p := proposal(t, ctx, pool, db, sqlA, inverseA)
	rec := mustPropose(t, ctx, s, p).Recommendation
	list, err := s.ListActionable(ctx, db)
	if err != nil || len(list) != 1 || list[0].ID != rec.ID || list[0].Current.ForwardSQL != sqlA {
		t.Fatalf("actionable = %+v, %v", list, err)
	}
	fresh, err := s.CheckFresh(ctx, list[0])
	if err != nil || !fresh.Fresh || fresh.Supersede {
		t.Fatalf("fresh = %+v, %v, want fresh", fresh, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status='resolved',
		resolved_at=now() WHERE id=$1`, *rec.FindingID); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.CheckFresh(ctx, list[0])
	if err != nil || fresh.Fresh || !fresh.Supersede || fresh.Reason == "" {
		t.Fatalf("closed finding: %+v, %v, want supersede with reason", fresh, err)
	}
	stale := list[0]
	stale.Revision = 2
	if fresh, _ := s.CheckFresh(ctx, stale); fresh.Fresh {
		t.Fatal("a candidate read at an old revision is fresh")
	}
	if other, _ := s.ListActionable(ctx, db+"_other"); len(other) != 0 {
		t.Fatalf("another database listed %d candidates", len(other))
	}
}

func TestListActionableHonoursBackoff(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	if _, err := s.RecordFailure(ctx, claim(t, ctx, s, rec), "x"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListActionable(ctx, rec.DatabaseName); len(list) != 0 {
		t.Fatalf("failed row listed during backoff: %d", len(list))
	}
	makeDue(t, ctx, pool, rec.ID)
	list, err := s.ListActionable(ctx, rec.DatabaseName)
	if err != nil || len(list) != 1 || list[0].State != StateFailed {
		t.Fatalf("due failed row: %+v, %v", list, err)
	}
}

func TestFindForOperator(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	found, err := s.FindForOperator(ctx, *rec.FindingID, sqlA)
	if err != nil || found == nil || found.ID != rec.ID || found.Current.InverseSQL != inverseA {
		t.Fatalf("find = %+v, %v", found, err)
	}
	if found, err := s.FindForOperator(ctx, *rec.FindingID, sqlA2); err != nil || found != nil {
		t.Fatalf("different SQL: %+v, %v, want none", found, err)
	}
}
