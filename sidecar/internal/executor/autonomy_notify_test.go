package executor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// The L2 one-click handoff reaches a human: queuing it raises one
// approval_needed notification, and a deduplicated repeat raises none.

type capturingDispatcher struct {
	mu     sync.Mutex
	events []notify.Event
}

func (d *capturingDispatcher) Dispatch(_ context.Context, e notify.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
	return nil
}

func TestL2HandoffNotifiesApprovalNeededOnce(t *testing.T) {
	exec, pool := handoffExecutor(t, 2)
	clearHandoffs(t, pool, freezeHandoffKey)
	d := &capturingDispatcher{}
	exec.WithDispatcher(d)
	exec.WithDatabaseName("orders")
	for i := 0; i < 2; i++ {
		if err := exec.SubmitCustodianProposal(context.Background(), freezeProposal()); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if len(d.events) != 1 || d.events[0].Type != "approval_needed" ||
		d.events[0].Data["database"] != "orders" ||
		!strings.Contains(d.events[0].Subject, "wraparound_runway") {
		t.Fatalf("notifications = %+v", d.events)
	}
}
