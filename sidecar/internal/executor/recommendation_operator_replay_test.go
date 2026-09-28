package executor

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// PR #52 CI: when two operator executions of one approval ran one after
// the other, the second found no claimable recommendation (the first had
// moved it to verifying) and ran the SQL again unclaimed. An approval must
// execute exactly once, however the calls interleave.
func TestExecuteManualSequentialReplayIsRefused(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.recs.Approve(fx.ctx, rec.ID, rec.ContentHash, "user:8"); err != nil {
		t.Fatal(err)
	}
	user := 8
	first, err := fx.exec.ExecuteManual(fx.ctx, int(fx.id), fx.sql, "", &user)
	if err != nil || first <= 0 {
		t.Fatalf("first ExecuteManual = %d, %v", first, err)
	}
	second, err := fx.exec.ExecuteManual(fx.ctx, int(fx.id), fx.sql, "", &user)
	if err == nil || second != 0 {
		t.Fatalf("replayed ExecuteManual = %d, %v; want it refused", second, err)
	}
	if rows := fx.actionRows(t); rows != 1 {
		t.Fatalf("action_log rows = %d, want exactly 1", rows)
	}
}

// The CI race window: the other operator's call has already claimed the
// recommendation (applying) when this one looks for it. Finding nothing
// claimable must not mean "run unclaimed": an in-flight or applied
// recommendation for the same finding and SQL refuses with ErrConflict.
func TestClaimForOperatorRefusesInFlightRecommendation(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.recs.Approve(fx.ctx, rec.ID, rec.ContentHash, "user:8"); err != nil {
		t.Fatal(err)
	}
	user := 8
	if _, err := fx.exec.claimForOperator(fx.ctx, int(fx.id), fx.sql, &user); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	claim, err := fx.exec.claimForOperator(fx.ctx, int(fx.id), fx.sql, &user)
	if !errors.Is(err, recommendation.ErrConflict) || claim != nil {
		t.Fatalf("claim while in flight = %v, %v; want ErrConflict", claim, err)
	}
}
