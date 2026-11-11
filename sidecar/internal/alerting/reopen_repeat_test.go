package alerting

import (
	"context"
	"testing"
	"time"
)

// alerting-07 review: a row an action resolved reopens with the same id
// when the condition persists (analyzer reopenAfterAction). Episode
// identity by finding id alone would treat that as the same episode and
// stay silent until the next reminder (4 h critical, 24 h warning, never
// for info) right after a "resolved" notification. The resolve ends the
// episode, so the reopen is alerted again once the cooldown has passed.
// No concurrency test beyond throttle_repeat_test.go: endEpisode takes
// the same mutex as every other throttle method.

func TestRepeat_SameRowReopenedAfterResolveIsSentAfterCooldown(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "k", "critical", 1)
	clock.set(5 * time.Minute)
	th.endEpisode("k", 1)
	clock.set(10 * time.Minute)
	wantDecision(t, "reopened inside the cooldown",
		first(cycle(th, "k", "critical", 1)), DecisionThrottled)
	clock.set(15 * time.Minute)
	d, openFor := cycle(th, "k", "critical", 1)
	wantDecision(t, "reopened once the cooldown passed", d, DecisionSend)
	if openFor != 0 {
		t.Errorf("reopen openFor = %v, want 0 (a first alert)", openFor)
	}
	clock.set(254 * time.Minute)
	wantDecision(t, "239m after the reopen alert",
		first(cycle(th, "k", "critical", 1)), DecisionThrottled)
	clock.set(255 * time.Minute)
	d, openFor = cycle(th, "k", "critical", 1)
	wantDecision(t, "240m after the reopen alert", d, DecisionRemind)
	if openFor != 240*time.Minute {
		t.Errorf("reminder openFor = %v, want 4h from the reopen alert", openFor)
	}
}

// Info has no reminders, so without the episode end a reopened info
// finding would never be alerted again.
func TestRepeat_InfoReopenedAfterResolveIsSentAgain(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "k", "info", 1)
	clock.set(7 * time.Hour)
	th.endEpisode("k", 1)
	wantDecision(t, "info reopened after its cooldown",
		first(cycle(th, "k", "info", 1)), DecisionSend)
}

func TestRepeat_EndEpisodeOfAnotherFindingKeepsRecord(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "k", "critical", 2)
	th.endEpisode("k", 1) // an older resolved row of the same identity
	th.endEpisode("k", 0) // no identity: never ends a record
	th.endEpisode("missing", 1)
	if _, ok := th.sent["missing"]; ok {
		t.Error("endEpisode created a record for an unknown key")
	}
	if got := th.sent["k"].episode; got != 2 {
		t.Fatalf("episode = %d, want 2 kept", got)
	}
	clock.set(time.Hour)
	wantDecision(t, "open episode 2 an hour in",
		first(th.DecideEpisode("k", "critical", 2)), DecisionThrottled)
	th.endEpisode("k", 2)
	th.endEpisode("k", 2) // idempotent
	if got := th.sent["k"].episode; got != endedEpisode {
		t.Errorf("episode = %d after its resolve, want %d", got, endedEpisode)
	}
}

// Through the Manager: alert, resolve notification, same row reopened.
func TestManager_ActionReopenedFindingAlertsAgain(t *testing.T) {
	ch := &scriptedChannel{}
	m, clock := managerOnClock(t, ch)
	f := persistentFinding()
	ctx := context.Background()
	m.dispatchFindings(ctx, []AlertFinding{f})
	clock.set(20 * time.Minute)
	m.resolveFinding(ctx, resolveTarget{finding: f,
		channels: map[string]bool{ch.Name(): true}, failures: map[string]int{}})
	clock.set(40 * time.Minute)
	m.dispatchFindings(ctx, []AlertFinding{f})
	if len(ch.alerts) != 3 {
		t.Fatalf("alerts = %d, want first, resolved and reopened", len(ch.alerts))
	}
	if !ch.alerts[1].Resolved {
		t.Errorf("second alert = %+v, want the resolve notification", ch.alerts[1])
	}
	if a := ch.alerts[2]; a.Resolved || a.Reminder || a.OpenFor != 0 {
		t.Errorf("reopened alert = resolved %v reminder %v openFor %v, want a first alert",
			a.Resolved, a.Reminder, a.OpenFor)
	}
}
