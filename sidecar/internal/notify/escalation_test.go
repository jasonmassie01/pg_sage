package notify

import (
	"context"
	"testing"
	"time"
)

// alerting-06 review: an alert that escalates while its lower-severity
// trigger still waits out quiet hours is sent at once, and the waiting
// trigger is dropped, so the channel never gets the stale warning after
// the critical page.
// No concurrent access tests here: the schedule lock is covered by
// TestOutbox_TwoDatabasesSameKeyHaveIndependentState.

func TestDecide_EscalationCancelsThePendingTrigger(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("warning"), p.Decide(nil, findingEvt("warning"), stateT0))
	if d := p.Decide(st, findingEvt("critical"), stateT0); !d.CancelPending || d.Suppressed {
		t.Fatalf("escalation = %+v, want a send that cancels the pending trigger", d)
	}
	if d := p.Decide(nil, findingEvt("critical"), stateT0); d.CancelPending {
		t.Fatalf("first alert = %+v, want no cancellation", d)
	}
}

func TestOutbox_EscalationDropsTheDeferredWarning(t *testing.T) {
	rig := newOutboxRig(t, "22:00", "02:00")
	rig.clock.now = time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	ctx := context.Background()
	if res, _ := rig.d.Submit(ctx, findingEvt("warning")); res.Enqueued != 1 {
		t.Fatalf("warning = %+v, want it queued for the end of quiet hours", res)
	}
	rig.clock.Advance(time.Hour)
	if res, _ := rig.d.Submit(ctx, findingEvt("critical")); res.Enqueued != 1 {
		t.Fatalf("escalation = %+v, want it queued", res)
	}
	if got := rig.drain(t); got.Sent != 1 {
		t.Fatalf("drain at once = %+v, want the critical sent", got)
	}
	rig.clock.now = time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC) // window end
	if got := rig.drain(t); got.Sent != 0 {
		t.Fatalf("drain at 02:00 = %+v, want the stale warning dropped", got)
	}
	if _, accepted := seq.counts(); accepted != 1 {
		t.Fatalf("accepted = %d (%v), want only the critical", accepted, seq.bodies)
	}
}
