package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood lifeos (stale approval): decision 326942 kept counting
// queue_approval / approval_required repeats for hours. The verdict is part
// of the fingerprint, so a changed verdict must open its own row and never
// be folded into the stale one; a repeat refreshes the reason.
func TestChangedVerdictOpensNewRowAndKeepsTheOldOne(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	queued := parkedInput(fmt.Sprintf("fpvc_%d", time.Now().UnixNano()), ledgerDatabaseID())
	queued.Verdict = VerdictQueueApproval
	queued.Reason = "approval_required"
	queued.Fingerprint = DecisionFingerprint(queued)
	cleanupFingerprint(t, pool, queued.Fingerprint)
	first, err := service.RecordDecision(ctx, queued)
	if err != nil {
		t.Fatalf("queue_approval decision: %v", err)
	}
	parked := queued
	parked.Verdict = VerdictPark
	parked.Reason = "outside maintenance window"
	parked.Fingerprint = DecisionFingerprint(parked)
	cleanupFingerprint(t, pool, parked.Fingerprint)
	second, err := service.RecordDecision(ctx, parked)
	if err != nil {
		t.Fatalf("parked decision: %v", err)
	}
	if second.ID == first.ID || parked.Fingerprint == queued.Fingerprint {
		t.Fatalf("parked verdict reused queue_approval row %d", first.ID)
	}
	if verdict := decisionVerdict(t, pool, first.ID); verdict != "queue_approval" {
		t.Fatalf("first row verdict = %q, want queue_approval kept", verdict)
	}
	if verdict := decisionVerdict(t, pool, second.ID); verdict != "parked" {
		t.Fatalf("second row verdict = %q, want parked", verdict)
	}
	queued.Reason = "approval_required (repeat)"
	again, err := service.RecordDecision(ctx, queued)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeat queue_approval = %d (%v), want open row %d", again.ID, err, first.ID)
	}
	repeats, _, reason := decisionRow(t, pool, first.ID)
	if repeats != 2 || reason != "approval_required (repeat)" {
		t.Fatalf("repeats=%d reason=%q, want 2 and the latest reason", repeats, reason)
	}
}

func decisionVerdict(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var verdict string
	if err := pool.QueryRow(context.Background(), `SELECT verdict FROM sage.decision
		WHERE id=$1`, id).Scan(&verdict); err != nil {
		t.Fatalf("read decision %d verdict: %v", id, err)
	}
	return verdict
}
