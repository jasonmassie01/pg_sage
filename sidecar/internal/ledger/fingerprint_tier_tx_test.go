package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// audit-11 review: the budget transaction records withheld decisions
// through NewTxRepository, where retierDecision's Begin is a savepoint.
// A re-tier there commits or rolls back with the caller's transaction.

// No concurrent access tests here: TestConcurrentRetieringKeepsOneOpenRow
// covers racing writers; this pins the savepoint path's state.

func TestRetierInsideTheCallersTransaction(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	input := parkedInput(fmt.Sprintf("fptx_%d", time.Now().UnixNano()), ledgerDatabaseID())
	input.RiskTier = "moderate"
	cleanupFingerprint(t, pool, input.Fingerprint)
	first, err := NewService(NewPostgresRepository(pool)).RecordDecision(ctx, input)
	if err != nil {
		t.Fatalf("record at moderate: %v", err)
	}
	retier := func(commit bool) int64 {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		in := input
		in.RiskTier = "high"
		d, err := NewService(NewTxRepository(tx)).RecordDecision(ctx, in)
		if err != nil || d.ID == first.ID {
			t.Fatalf("re-tier in a transaction: id %d (first %d), %v", d.ID, first.ID, err)
		}
		end := tx.Rollback
		if commit {
			end = tx.Commit
		}
		if err := end(ctx); err != nil {
			t.Fatalf("end transaction (commit %v): %v", commit, err)
		}
		return d.ID
	}
	retier(false)
	got := tierRows(t, pool, input.Fingerprint)
	if want := []tierRow{{first.ID, "moderate", 1, false}}; fmt.Sprint(got) !=
		fmt.Sprint(want) {
		t.Fatalf("after a rolled-back re-tier rows = %+v, want %+v", got, want)
	}
	second := retier(true)
	got = tierRows(t, pool, input.Fingerprint)
	want := []tierRow{{first.ID, "moderate", 1, true}, {second, "high", 1, false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after a committed re-tier rows = %+v, want %+v", got, want)
	}
}
