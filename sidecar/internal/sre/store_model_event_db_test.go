package sre

import (
	"errors"
	"strings"
	"testing"
)

// RecordEvent appends a model-turn event to the hash chain under the
// lease. Only the model event types are accepted, a stale lease commits
// nothing, and payloads are bounded.

func TestRecordEvent_AppendsModelEventsUnderTheLease(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 90")
	for _, typ := range []string{EventModelReviewed, EventModelRejected,
		EventModelDisagreed} {
		if err := st.RecordEvent(ctx, lease, typ, map[string]any{"reason": "x"}); err != nil {
			t.Fatalf("record %s: %v", typ, err)
		}
	}
	events, err := st.Events(ctx, lease.Scope, lease.InvestigationID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	last := events[len(events)-3:]
	if last[0].Type != EventModelReviewed || last[2].Type != EventModelDisagreed ||
		!strings.Contains(last[1].Actor, string(lease.WorkerID)) {
		t.Fatalf("events = %+v", last)
	}
	if err := st.VerifyEvents(ctx, lease.Scope, lease.InvestigationID); err != nil {
		t.Fatalf("chain: %v", err)
	}
}

func TestRecordEvent_Negatives(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 91")
	for _, typ := range []string{EventConcluded, EventPinned, "", "model_executed_sql"} {
		if err := st.RecordEvent(ctx, lease, typ, nil); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("type %q = %v, want ErrInvalidRequest", typ, err)
		}
	}
	big := map[string]any{"detail": strings.Repeat("x", 20000)}
	if err := st.RecordEvent(ctx, lease, EventModelRejected, big); !errors.Is(err,
		ErrInvalidRequest) {
		t.Errorf("oversized payload = %v, want ErrInvalidRequest", err)
	}
	stale := lease
	stale.Fence++
	if err := st.RecordEvent(ctx, stale, EventModelRejected, nil); !errors.Is(err,
		ErrLeaseLost) {
		t.Errorf("stale fence = %v, want ErrLeaseLost", err)
	}
	other := lease
	other.WorkerID = NewUUID()
	if err := st.RecordEvent(ctx, other, EventModelRejected, nil); !errors.Is(err,
		ErrLeaseLost) {
		t.Errorf("other worker = %v, want ErrLeaseLost", err)
	}
	before, _ := st.Events(ctx, lease.Scope, lease.InvestigationID)
	for _, e := range before {
		if strings.HasPrefix(e.Type, "model_") {
			t.Fatalf("a refused event was written: %+v", e)
		}
	}
}
