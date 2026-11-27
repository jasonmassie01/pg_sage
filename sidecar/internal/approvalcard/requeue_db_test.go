package approvalcard

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/store"
)

// policy-02 (CHECK-02): every card of a re-queued item shows the gate's
// reason. Linking a decision to its queue item resolves the decision, so
// the gate's next equal verdict is a new row that the re-queue links.
func TestRequeuedCardShowsTheGateReason(t *testing.T) {
	pool, ctx := livePool(t)
	q1, findingID := queueOptimizerIndex(t, ctx, pool)
	var sql string
	if err := pool.QueryRow(ctx, `SELECT proposed_sql FROM sage.action_queue
		WHERE id = $1`, q1).Scan(&sql); err != nil {
		t.Fatal(err)
	}
	repo := ledger.NewPostgresRepository(pool)
	service := ledger.NewService(repo)
	database := 970000 + int(time.Now().UnixNano()%20000)
	input := ledger.DecisionInput{DatabaseID: &database, Feature: "optimizer",
		Intent: "create_index", ProposedSQL: sql, Verdict: ledger.VerdictQueueApproval,
		Reason: "approval_required", RiskTier: "moderate", PolicyVersion: 1,
		TargetObjects: []string{fmt.Sprintf("public.orders|requeue_%d", findingID)}}
	input.Fingerprint = ledger.DecisionFingerprint(input)
	first, err := service.RecordDecision(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if linked, err := repo.LinkQueue(ctx, first.ID, q1); err != nil || !linked {
		t.Fatalf("link Q1 = %v, %v", linked, err)
	}
	if err := store.NewActionStore(pool).Reject(ctx, q1, 1, "not now"); err != nil {
		t.Fatal(err)
	}
	q2, err := store.NewActionStore(pool).Propose(ctx, nil, findingID, sql, "", "moderate")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RecordDecision(ctx, input)
	if err != nil || second.ID == first.ID {
		t.Fatalf("re-queue decision = %d (%v), want a new row after %d", second.ID, err,
			first.ID)
	}
	if linked, err := repo.LinkQueue(ctx, second.ID, q2); err != nil || !linked {
		t.Fatalf("link Q2 = %v, %v", linked, err)
	}
	for _, queueID := range []int{q1, q2} {
		c, err := Loader{Pool: pool, Database: "orders"}.Card(ctx, queueID)
		if err != nil {
			t.Fatalf("card %d: %v", queueID, err)
		}
		if !hasCode(c, "gate:approval_required") || c.Risk.Tier != "moderate" {
			t.Fatalf("card %d why=%v tier=%q, want the gate reason at moderate", queueID,
				reasonCodes(c), c.Risk.Tier)
		}
	}
}

// A decision already linked keeps its link, and invalid ids link nothing.
func TestLinkQueueKeepsTheFirstLink(t *testing.T) {
	pool, ctx := livePool(t)
	q1, _ := queueOptimizerIndex(t, ctx, pool)
	q2, _ := queueOptimizerIndex(t, ctx, pool)
	repo := ledger.NewPostgresRepository(pool)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('optimizer', 'x', '[]', 'queue_approval', 'moderate', 'approval_required',
		'{}', $1) RETURNING id`, fmt.Sprintf("ev-keep-%d", q1)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if linked, err := repo.LinkQueue(ctx, id, q1); err != nil || !linked {
		t.Fatalf("first link = %v, %v", linked, err)
	}
	if linked, err := repo.LinkQueue(ctx, id, q2); err != nil || linked {
		t.Fatalf("second link = %v, %v, want kept", linked, err)
	}
	for _, bad := range [][2]int64{{0, int64(q1)}, {id, 0}, {-1, -1}} {
		if linked, err := repo.LinkQueue(ctx, bad[0], int(bad[1])); err != nil || linked {
			t.Fatalf("link %v = %v, %v, want nothing", bad, linked, err)
		}
	}
	var queueID int
	var resolved bool
	if err := pool.QueryRow(ctx, `SELECT queue_id, resolved_at IS NOT NULL
		FROM sage.decision WHERE id = $1`, id).Scan(&queueID, &resolved); err != nil {
		t.Fatal(err)
	}
	if queueID != q1 || !resolved {
		t.Fatalf("decision queue_id=%d resolved=%v, want %d and resolved", queueID,
			resolved, q1)
	}
}
