package sre

import (
	"context"
	"testing"
	"time"
)

// sre-08 review: slow follow-up deliveries must not hold the coordinator
// loop (and so new investigations) for the whole batch. Once a tick's
// budget is spent, the remaining candidates stay unclaimed and are sent on
// the next tick.

// slowSink advances the coordinator's clock past the tick budget on every
// delivery, as a channel that times out would.
type slowSink struct {
	recordingSink
	clock *time.Time
}

func (s *slowSink) DeliverConclusion(ctx context.Context, sig ConclusionSignal) error {
	*s.clock = s.clock.Add(conclusionTickBudget + time.Second)
	return s.recordingSink.DeliverConclusion(ctx, sig)
}

func TestConclusionNotifier_SlowDeliveriesYieldTheTick(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	clock := time.Now()
	sink := &slowSink{clock: &clock}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	c.now = func() time.Time { return clock }
	var incidents []string
	for i := 0; i < 2; i++ {
		inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
		incidents = append(incidents, inc)
		a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root")
		ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
	}
	c.notifyConclusions(ctx, scope)
	first := sink.signals()
	if len(first) != 1 {
		t.Fatalf("one tick sent %d follow-ups past its budget, want 1", len(first))
	}
	var waiting string
	for _, inc := range incidents {
		if inc != first[0].IncidentID {
			waiting = inc
		}
	}
	if _, claimed := claimOf(t, ctx, pool, waiting); claimed {
		t.Fatalf("incident %s was claimed although its follow-up was deferred", waiting)
	}
	c.notifyConclusions(ctx, scope)
	got := sink.signals()
	if len(got) != 2 || got[1].IncidentID != waiting {
		t.Fatalf("the next tick sent %+v, want the deferred incident %s", got, waiting)
	}
	if claim, ok := claimOf(t, ctx, pool, waiting); !ok || !claim.delivered {
		t.Fatalf("deferred incident claim = %+v (%v), want delivered", claim, ok)
	}
}
