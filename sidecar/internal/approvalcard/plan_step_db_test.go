package approvalcard

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// plan-02a: a plan step's queue item is decided on its plan (plan-03),
// never on a card: the notifier sends no approval request for it, Card
// refuses it and the pending cards leave it out. Other items keep theirs.
func TestNotifierSendsNoCardForAPlanStep(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	other, _ := queueOptimizerIndex(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET identity_key = 'plan_step:4:9',
		proposed_by = 'plan:4' WHERE id = $1`, queueID); err != nil {
		t.Fatalf("mark the plan step: %v", err)
	}
	inner := &captureDispatch{}
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"}
	n := NewNotifier(inner, l)
	evt := notify.QueuedApprovalEvent("Plan 4 step 1", "CREATE INDEX ...", "orders",
		"moderate", queueID)
	if err := n.Dispatch(ctx, evt); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(inner.events) != 0 {
		t.Fatalf("the notifier sent %d events for a plan step, want none", len(inner.events))
	}
	if _, err := l.Card(ctx, queueID); !errors.Is(err, ErrPlanStepItem) {
		t.Fatalf("Card of a plan step = %v, want ErrPlanStepItem", err)
	}
	cards, err := l.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, c := range cards {
		if c.QueueID == queueID {
			t.Fatalf("the pending cards include plan step item %d", queueID)
		}
	}
	if err := n.Dispatch(ctx, notify.QueuedApprovalEvent("Index", "CREATE INDEX ...",
		"orders", "moderate", other)); err != nil {
		t.Fatalf("dispatch other: %v", err)
	}
	if _, ok := notify.ApprovalCardOf(inner.last(t)); !ok {
		t.Fatal("an ordinary queue item lost its card")
	}
}
