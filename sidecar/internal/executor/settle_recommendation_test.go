package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/verify"
)

// verifyingRecAction takes a fresh recommendation to verifying with a
// decided, still-monitored action, as an apply leaves it before its
// verdict, and returns the head and the action.
func verifyingRecAction(t *testing.T) (*recFixture, recommendation.Recommendation, int64) {
	t.Helper()
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	approved, err := fx.recs.Approve(fx.ctx, rec.ID, rec.ContentHash, "user:1")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	c, err := fx.recs.Claim(fx.ctx, recommendation.ClaimRequest{ID: approved.ID,
		Revision: approved.Revision, Lease: time.Hour})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	decisionID := recordCustodianDecision(t, fx.ctx, fx.pool, "rec_settle", fx.table)
	var actionID int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO sage.action_log (action_type,
		finding_id, sql_executed, outcome, decision_id) VALUES ('ddl', $1, $2,
		'monitoring', $3) RETURNING id`, fx.id, fx.sql, decisionID).Scan(&actionID); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	if err := fx.recs.RecordApplied(fx.ctx, c, actionID); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	if err := fx.recs.StartVerifying(fx.ctx, c); err != nil {
		t.Fatalf("start verifying: %v", err)
	}
	return fx, approved, actionID
}

type settleCase struct {
	verdict, verification, recVerdict string
	want                              recommendation.State
}

// cases-03: the executor settles a neutral verdict as action outcome
// success with a durable unverifiable verification. The recommendation
// must end inconclusive, never verified; only an improvement verifies it.
func TestSettleOutcomeEndsRecommendationByVerdict(t *testing.T) {
	cases := []settleCase{
		{verify.OutcomeNeutral, "unverifiable", "unverifiable",
			recommendation.StateInconclusive},
		{verify.OutcomeImproved, "success", "success", recommendation.StateVerified},
	}
	for _, tc := range cases {
		// One subtest per case: each fixture holds the cross-package test
		// lock until its own cleanup.
		t.Run(tc.verdict, func(t *testing.T) { settleAndCheck(t, tc) })
	}
}

func settleAndCheck(t *testing.T, tc settleCase) {
	fx, rec, actionID := verifyingRecAction(t)
	o := verify.Outcome{ActionLogID: actionID, Verdict: tc.verdict, Reason: "settle test"}
	if !settleOutcome(fx.ctx, fx.pool, o, nopLog) {
		t.Fatal("settleOutcome reported the action changed state")
	}
	var outcome, verification string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT al.outcome, v.verdict
		FROM sage.action_log al JOIN sage.verification v ON v.id = al.verification_id
		WHERE al.id=$1`, actionID).Scan(&outcome, &verification); err != nil {
		t.Fatalf("read settled action: %v", err)
	}
	if outcome != "success" || verification != tc.verification {
		t.Fatalf("action outcome=%s verification=%s, want success/%s",
			outcome, verification, tc.verification)
	}
	if _, err := fx.recs.ReconcileVerifying(fx.ctx, fx.exec.databaseName); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := fx.get(t, rec.ID); got.State != tc.want || got.Verdict != tc.recVerdict {
		t.Errorf("recommendation state=%s verdict=%q, want %s %q",
			got.State, got.Verdict, tc.want, tc.recVerdict)
	}
}
