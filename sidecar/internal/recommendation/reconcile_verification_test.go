package recommendation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// verifiedAction makes a verifying head whose action carries a decision,
// as executor actions do, and returns it with its action and decision.
func verifiedAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Store, outcome string,
) (Recommendation, int64, int64) {
	t.Helper()
	rec, actionID := verifyingWithOutcome(t, ctx, pool, s, outcome)
	var decisionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		verdict, risk_tier, reason, evidence_id) VALUES ('t', 'i', 'execute', 'safe',
		'r', md5(random()::text)) RETURNING id`).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET decision_id=$2
		WHERE id=$1`, actionID, decisionID); err != nil {
		t.Fatal(err)
	}
	return rec, actionID, decisionID
}

// linkVerification inserts a verification row with verdict (for the
// action when visible, else unattached) and links it from the action, as
// the verifier and the executor's post-check do at once.
func linkVerification(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	actionID, decisionID int64, verdict string, visible bool,
) int64 {
	t.Helper()
	var owner *int64
	if visible {
		owner = &actionID
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.verification (decision_id,
		action_log_id, criterion, baseline, minimum_samples, next_evaluation_at,
		hard_deadline_at, verdict) VALUES ($1, $2, '{}', '{}', 1, now(), now(), $3)
		RETURNING id`, decisionID, owner, verdict).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET verification_id=$2
		WHERE id=$1`, actionID, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func reconcileAndGet(
	t *testing.T, ctx context.Context, s *Store, rec Recommendation,
) Recommendation {
	t.Helper()
	if _, err := s.ReconcileVerifying(ctx, rec.DatabaseName); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return mustGet(t, ctx, s, rec.ID)
}

// cases-03: a success outcome settles by the durable verification verdict.
// Only a verifier success is verified; the neutral (unverifiable) and
// failed verdicts are inconclusive; revert, pending and extended keep
// verifying.
func TestReconcileSettlesSuccessByVerificationVerdict(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	cases := []struct {
		verification, verdict string
		want                  State
	}{
		{"success", "success", StateVerified},
		{"unverifiable", "unverifiable", StateInconclusive},
		{"failed", "failed", StateInconclusive},
		{"revert", "regressed", StateVerifying},
		{"pending", "", StateVerifying},
		{"extended", "", StateVerifying},
	}
	for _, tc := range cases {
		rec, actionID, decisionID := verifiedAction(t, ctx, pool, s, "success")
		linkVerification(t, ctx, pool, actionID, decisionID, tc.verification, true)
		got := reconcileAndGet(t, ctx, s, rec)
		if got.State != tc.want || got.Verdict != tc.verdict {
			t.Errorf("success + %s: state=%s verdict=%q, want %s %q",
				tc.verification, got.State, got.Verdict, tc.want, tc.verdict)
		}
	}
}

// A linked verification row that this read cannot see yet keeps the head
// verifying; it settles once the row is visible and complete.
func TestReconcileWaitsForLinkedVerification(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec, actionID, decisionID := verifiedAction(t, ctx, pool, s, "success")
	vid := linkVerification(t, ctx, pool, actionID, decisionID, "pending", false)
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateVerifying {
		t.Fatalf("linked but not visible: state=%s, want verifying", got.State)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.verification SET action_log_id=$2
		WHERE id=$1`, vid, actionID); err != nil {
		t.Fatal(err)
	}
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateVerifying {
		t.Fatalf("pending verification: state=%s, want verifying", got.State)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.verification SET verdict='success',
		completed_at=now() WHERE id=$1`, vid); err != nil {
		t.Fatal(err)
	}
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateVerified ||
		got.Verdict != "success" {
		t.Fatalf("after success: state=%s verdict=%q, want verified/success",
			got.State, got.Verdict)
	}
}

// The executor's post-check writes the outcome, then the verification row
// in a second statement. A decided action measured moments ago with no
// row yet keeps verifying, so a neutral verdict landing just after the
// outcome still ends inconclusive, never verified early.
func TestReconcileWaitsForPostCheckRowJustAfterOutcome(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec, actionID, decisionID := verifiedAction(t, ctx, pool, s, "success")
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET measured_at=now()
		WHERE id=$1`, actionID); err != nil {
		t.Fatal(err)
	}
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateVerifying {
		t.Fatalf("outcome written, row not yet: state=%s, want verifying", got.State)
	}
	linkVerification(t, ctx, pool, actionID, decisionID, "unverifiable", true)
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateInconclusive ||
		got.Verdict != "unverifiable" {
		t.Fatalf("after the neutral row: state=%s verdict=%q, want "+
			"inconclusive/unverifiable", got.State, got.Verdict)
	}
}

// Boundary: past the grace window a decided success with no verification
// row at all (an action whose own post-check is its evidence) is verified,
// so such heads never stay verifying forever.
func TestReconcileVerifiesPostCheckSuccessAfterGrace(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec, actionID, _ := verifiedAction(t, ctx, pool, s, "success")
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log
		SET measured_at = now() - interval '2 minutes' WHERE id=$1`, actionID); err != nil {
		t.Fatal(err)
	}
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateVerified ||
		got.Verdict != "success" {
		t.Fatalf("post-check success after grace: state=%s verdict=%q, want "+
			"verified/success", got.State, got.Verdict)
	}
}

// Nil: a decided success whose measured_at is NULL and that has no
// verification row has no row on the way, so the grace expression must
// read NULL as "not expected" and verify the head rather than hold it.
func TestReconcileVerifiesDecidedSuccessWithoutMeasuredAt(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec, actionID, _ := verifiedAction(t, ctx, pool, s, "success")
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET measured_at = NULL
		WHERE id=$1`, actionID); err != nil {
		t.Fatal(err)
	}
	if got := reconcileAndGet(t, ctx, s, rec); got.State != StateVerified ||
		got.Verdict != "success" {
		t.Fatalf("decided success, measured_at NULL: state=%s verdict=%q, want "+
			"verified/success", got.State, got.Verdict)
	}
}
