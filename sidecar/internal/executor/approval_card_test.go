package executor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Approval cards (roadmap 1.5): the executor's approval request names its
// queue item (so a card can be built and approved in chat), the gate
// decision that queued it is linked to the item (the card's "why"), and an
// operator's snooze keeps the change behind approval like a rejection
// does, until the snooze ends.

type eventCapture struct {
	mu     sync.Mutex
	events []notify.Event
}

func (c *eventCapture) Dispatch(_ context.Context, evt notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
	return nil
}

func TestClassifySnoozedPendingBlocksAutonomy(t *testing.T) {
	const sql = "CREATE INDEX CONCURRENTLY i ON public.t (c);"
	rows := []queuedApproval{{ID: 7, Status: "pending", SQL: sql, Snoozed: true}}
	ids, blocked := classifyQueuedApprovals(rows, sql, nil)
	if len(ids) != 0 || !strings.Contains(blocked, "snoozed") ||
		!strings.Contains(blocked, "7") {
		t.Fatalf("supersede=%v blocked=%q, want blocked by the snooze", ids, blocked)
	}
	// A snooze of other content is the existing "different content" block.
	other := []queuedApproval{{ID: 8, Status: "pending", SQL: "VACUUM t", Snoozed: true}}
	if _, blocked := classifyQueuedApprovals(other, sql, nil); !strings.Contains(blocked,
		"different content") {
		t.Fatalf("blocked = %q", blocked)
	}
	// An ended snooze (Snoozed false) is superseded as before.
	ended := []queuedApproval{{ID: 9, Status: "pending", SQL: sql}}
	if ids, blocked := classifyQueuedApprovals(ended, sql, nil); blocked != "" ||
		len(ids) != 1 || ids[0] != 9 {
		t.Fatalf("ids=%v blocked=%q", ids, blocked)
	}
}

func TestQueuedApprovalNamesItsQueueItemAndLinksTheDecision(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	capture := &eventCapture{}
	fx.exec.WithDispatcher(capture)
	q := fx.queuePending(t)
	capture.mu.Lock()
	var found *notify.Event
	for i := range capture.events {
		if capture.events[i].Type == "approval_needed" {
			found = &capture.events[i]
		}
	}
	capture.mu.Unlock()
	if found == nil || found.Data["queue_id"] != q.ID || found.Data["database"] != fx.database {
		t.Fatalf("approval event = %+v, want queue_id %d", found, q.ID)
	}
	var linked int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM sage.decision
		WHERE queue_id = $1 AND verdict = 'queue_approval'`, q.ID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("decisions linked to queue item %d = %d, want 1", q.ID, linked)
	}
}

func TestStaleApprovalSnoozedStaysBehindApproval(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	fx.setQueue(t, q.ID, "snoozed_until = now() + interval '1 hour', snoozed_by = 1, "+
		"snooze_reason = 'busy hours'")
	fx.markVerified(t)
	for range 2 {
		fx.exec.RunCycle(fx.ctx, false)
	}
	after := fx.onlyQueueRow(t)
	if after.Status != "pending" || after.ID != q.ID {
		t.Fatalf("queue row = %+v, want the snoozed proposal still pending", after)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.indexExists(t, fx.index()) {
		t.Fatal("a snoozed change ran autonomously")
	}
}

func TestLinkDecisionToQueueIsIdempotentAndSafe(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	var decisionID int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('optimizer', 'x', '[]', 'queue_approval', 'moderate', 'approval_required',
		'{}', 'ev-link-' || clock_timestamp()::text) RETURNING id`).
		Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	q := fx.queuePending(t)
	fx.exec.linkDecisionToQueue(fx.ctx, decisionID, q.ID)
	fx.exec.linkDecisionToQueue(fx.ctx, decisionID, q.ID+1000000) // already linked: kept
	fx.exec.linkDecisionToQueue(fx.ctx, 0, q.ID)                  // nothing to link
	var got *int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT queue_id FROM sage.decision WHERE id = $1`,
		decisionID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != q.ID {
		t.Fatalf("decision %d queue_id = %v, want %d", decisionID, got, q.ID)
	}
}

// linkedDecision is a sage.decision row linked to a queue item.
type linkedDecision struct {
	ID       int64
	QueueID  int
	Reason   string
	Tier     string
	Resolved bool
}

func (fx *staleFixture) linkedDecisions(t *testing.T, queueIDs ...int) []linkedDecision {
	t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT id, queue_id, reason, risk_tier,
		resolved_at IS NOT NULL FROM sage.decision WHERE queue_id = ANY($1) ORDER BY id`,
		queueIDs)
	if err != nil {
		t.Fatalf("read linked decisions: %v", err)
	}
	defer rows.Close()
	var out []linkedDecision
	for rows.Next() {
		var d linkedDecision
		if err := rows.Scan(&d.ID, &d.QueueID, &d.Reason, &d.Tier, &d.Resolved); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// policy-02: a queue item owns the verdict that queued it. After an
// operator rejects Q1 and the cooldown passes, the re-queued Q2 gets its
// own decision row, so Q2's card still says why the gate wants a person
// (the loader reads the newest decision linked to the queue item).
func TestRequeuedItemLinksItsOwnDecision(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q1 := fx.queuePending(t)
	cooldown := fx.exec.cascadeCooldown() + time.Hour
	fx.setQueue(t, q1.ID, "status = 'rejected', decided_by = 1, reason = 'not now', "+
		"decided_at = now() - make_interval(secs => $2)", cooldown.Seconds())
	fx.exec.RunCycle(fx.ctx, false)
	queue := fx.queueRows(t)
	if len(queue) != 2 || queue[0].ID != q1.ID || queue[1].Status != "pending" {
		t.Fatalf("queue = %+v, want rejected Q1 and a pending re-queue", queue)
	}
	q2 := queue[1]
	linked := fx.linkedDecisions(t, q1.ID, q2.ID)
	if len(linked) != 2 || linked[0].QueueID != q1.ID || linked[1].QueueID != q2.ID {
		t.Fatalf("linked decisions = %+v, want one row for Q1 and one for Q2", linked)
	}
	for _, d := range linked {
		if !d.Resolved { // CHECK-01
			t.Fatalf("decision %+v is linked but open: the next queue would fold into it", d)
		}
	}
	var reason, tier string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT reason, risk_tier FROM sage.decision
		WHERE queue_id = $1 ORDER BY id DESC LIMIT 1`, q2.ID).Scan(&reason, &tier); err != nil {
		t.Fatalf("Q2's card decision: %v", err)
	}
	if reason != string(policy.ReasonApprovalRequired) || tier != linked[0].Tier ||
		tier == "" {
		t.Fatalf("Q2's card decision reason=%q tier=%q, want approval_required at %q",
			reason, tier, linked[0].Tier)
	}
}

// State transition: linking resolves the decision, so the next equal
// verdict opens a new row instead of folding into the linked one.
func TestLinkedDecisionIsResolvedAndTheNextVerdictIsNew(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	linked := fx.linkedDecisions(t, q.ID)
	if len(linked) != 1 || !linked[0].Resolved {
		t.Fatalf("linked = %+v, want one resolved decision", linked)
	}
	fx.exec.RunCycle(fx.ctx, false) // Q1 still pending: the gate decides again
	var open int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM sage.decision
		WHERE target_objects = $1::jsonb AND verdict = 'queue_approval'
		  AND resolved_at IS NULL AND queue_id IS NULL AND id > $2`,
		`["`+fx.f.ObjectIdentifier+`"]`, linked[0].ID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("open unlinked queue verdicts after the link = %d, want 1 new row", open)
	}
}
