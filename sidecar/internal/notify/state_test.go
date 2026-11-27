package notify

import (
	"testing"
	"time"
)

// alerting-06 M3: per (database, dedup_key, channel) state decides
// cooldown, quiet hours (critical bypasses, others are deferred) and
// reminders at repeat_interval (0 = never). Pure functions over a clock.
// No concurrent access tests here: Decide and Apply are pure; the
// concurrency of the stored state is covered in outbox_test.go.

var stateT0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func testPolicy(t *testing.T, quietStart, quietEnd string) AlertPolicy {
	t.Helper()
	p, warns := NewAlertPolicy(15, map[string]int{"critical": 240, "warning": 60, "info": 0},
		quietStart, quietEnd, "UTC")
	if len(warns) != 0 {
		t.Fatalf("unexpected policy warnings: %v", warns)
	}
	return p
}

func findingEvt(sev string) Event {
	return Event{Type: "finding_opened", Severity: sev, Subject: "Bloat on t",
		DedupKey: "finding:db1:bloat:t", Data: map[string]any{"database": "db1"}}
}

func TestDecide_FirstAlertSendsNow(t *testing.T) {
	p := testPolicy(t, "", "")
	d := p.Decide(nil, findingEvt("warning"), stateT0)
	if d.Suppressed || !d.At.Equal(stateT0) || d.Reminder {
		t.Fatalf("decision = %+v, want an immediate send", d)
	}
}

func TestDecide_OpenSameSeverityIsSuppressed(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("warning"), p.Decide(nil, findingEvt("warning"), stateT0))
	d := p.Decide(st, findingEvt("warning"), stateT0.Add(10*time.Minute))
	if !d.Suppressed || d.Reason == "" {
		t.Fatalf("decision = %+v, want suppressed with a reason", d)
	}
}

func TestDecide_EscalationSendsAtOnce(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("warning"), p.Decide(nil, findingEvt("warning"), stateT0))
	at := stateT0.Add(time.Minute)
	d := p.Decide(st, findingEvt("critical"), at)
	if d.Suppressed || !d.At.Equal(at) {
		t.Fatalf("decision = %+v, want an escalation sent at %v", d, at)
	}
	st = p.Apply(st, findingEvt("critical"), d)
	if st.Severity != "critical" || !st.FirstSentAt.Equal(stateT0) {
		t.Fatalf("state = %+v, want critical, same episode start", st)
	}
}

func TestDecide_QuietHoursDeferWarningUntilWindowEnd(t *testing.T) {
	p := testPolicy(t, "22:00", "06:00")
	night := time.Date(2026, 11, 12, 23, 30, 0, 0, time.UTC)
	d := p.Decide(nil, findingEvt("warning"), night)
	want := time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	if d.Suppressed || !d.At.Equal(want) {
		t.Fatalf("decision = %+v, want deferred to %v", d, want)
	}
	// A second warning inside the window is the same alert: not sent twice.
	st := p.Apply(nil, findingEvt("warning"), d)
	again := p.Decide(st, findingEvt("warning"), night.Add(time.Hour))
	if !again.Suppressed {
		t.Fatalf("second warning in quiet hours = %+v, want suppressed", again)
	}
}

func TestDecide_CriticalBypassesQuietHours(t *testing.T) {
	p := testPolicy(t, "22:00", "06:00")
	night := time.Date(2026, 11, 12, 23, 30, 0, 0, time.UTC)
	d := p.Decide(nil, findingEvt("critical"), night)
	if d.Suppressed || !d.At.Equal(night) {
		t.Fatalf("decision = %+v, want sent at once", d)
	}
}

func TestDecide_QuietWindowEdges(t *testing.T) {
	p := testPolicy(t, "22:00", "06:00")
	start := time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC)
	if d := p.Decide(nil, findingEvt("info"), start); d.At.Equal(start) {
		t.Fatalf("22:00 is inside the window, got %+v", d)
	}
	end := time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	if d := p.Decide(nil, findingEvt("info"), end); !d.At.Equal(end) {
		t.Fatalf("06:00 ends the window, got %+v", d)
	}
}

func TestDecideReminder_ExactlyAtRepeatIntervalSendsOne(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("warning"), p.Decide(nil, findingEvt("warning"), stateT0))
	if d := p.DecideReminder(st, stateT0.Add(59*time.Minute)); !d.Suppressed {
		t.Fatalf("reminder before the interval = %+v, want suppressed", d)
	}
	at := stateT0.Add(60 * time.Minute)
	d := p.DecideReminder(st, at)
	if d.Suppressed || !d.Reminder || !d.At.Equal(at) {
		t.Fatalf("reminder at exactly 60m = %+v, want one reminder now", d)
	}
	st = p.Apply(st, findingEvt("warning"), d)
	if st.Reminders != 1 || !st.LastSentAt.Equal(at) {
		t.Fatalf("state after reminder = %+v, want 1 reminder at %v", st, at)
	}
	if again := p.DecideReminder(st, at); !again.Suppressed {
		t.Fatalf("second reminder at the same instant = %+v, want suppressed", again)
	}
}

func TestDecideReminder_ZeroRepeatNeverReminds(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("info"), p.Decide(nil, findingEvt("info"), stateT0))
	if d := p.DecideReminder(st, stateT0.Add(365*24*time.Hour)); !d.Suppressed {
		t.Fatalf("info with repeat 0 reminded: %+v", d)
	}
	if d := p.DecideReminder(nil, stateT0); !d.Suppressed {
		t.Fatalf("nil state reminded: %+v", d)
	}
}

func TestDecide_ResolveClosesAndDuplicateIsSuppressed(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("critical"), p.Decide(nil, findingEvt("critical"), stateT0))
	res := ResolvedEvent(findingEvt("critical"))
	d := p.Decide(st, res, stateT0.Add(time.Hour))
	if d.Suppressed {
		t.Fatalf("resolve of an open alert suppressed: %+v", d)
	}
	st = p.Apply(st, res, d)
	if st.Open {
		t.Fatalf("state still open after resolve: %+v", st)
	}
	if again := p.Decide(st, res, stateT0.Add(2*time.Hour)); !again.Suppressed {
		t.Fatalf("second resolve = %+v, want suppressed", again)
	}
}

func TestDecide_ResolveWithoutStateStillSends(t *testing.T) {
	// An incident paged before the upgrade has no state row; its resolve
	// must still reach PagerDuty under the key it was opened with.
	p := testPolicy(t, "", "")
	res := ResolvedEvent(Event{Type: "finding_critical", Severity: "critical",
		Subject: "x", DedupKey: "finding_critical:db1:bloat:t"})
	if d := p.Decide(nil, res, stateT0); d.Suppressed {
		t.Fatalf("legacy resolve suppressed: %+v", d)
	}
}

func TestDecide_ResolveOfPendingTriggerCancelsBoth(t *testing.T) {
	p := testPolicy(t, "22:00", "06:00")
	night := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	st := p.Apply(nil, findingEvt("warning"), p.Decide(nil, findingEvt("warning"), night))
	d := p.Decide(st, ResolvedEvent(findingEvt("warning")), night.Add(time.Hour))
	if !d.Suppressed || !d.CancelPending {
		t.Fatalf("resolve before the deferred send = %+v, want cancel and suppress", d)
	}
}

func TestDecide_ReopenWithinCooldownIsDeferredNotDropped(t *testing.T) {
	p := testPolicy(t, "", "")
	st := p.Apply(nil, findingEvt("warning"), p.Decide(nil, findingEvt("warning"), stateT0))
	res := ResolvedEvent(findingEvt("warning"))
	st = p.Apply(st, res, p.Decide(st, res, stateT0.Add(5*time.Minute)))
	d := p.Decide(st, findingEvt("warning"), stateT0.Add(10*time.Minute))
	want := stateT0.Add(30 * time.Minute) // warning floor 30m > cooldown_minutes 15
	if d.Suppressed || !d.At.Equal(want) {
		t.Fatalf("reopen in cooldown = %+v, want deferred to %v", d, want)
	}
}

func TestNewAlertPolicy_InvalidValuesWarnAndDisable(t *testing.T) {
	p, warns := NewAlertPolicy(0, nil, "25:00", "06:00", "Mars/Olympus")
	if len(warns) != 2 {
		t.Fatalf("warnings = %v, want timezone and quiet hours", warns)
	}
	night := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	if d := p.Decide(nil, findingEvt("info"), night); !d.At.Equal(night) {
		t.Fatalf("invalid quiet hours still deferred: %+v", d)
	}
	// Zero cooldown_minutes keeps the floors, never zero (default masking).
	if got := p.cooldownFor("warning"); got != 30*time.Minute {
		t.Fatalf("warning cooldown = %v, want the 30m floor", got)
	}
	if got := p.repeatFor("critical"); got != 240*time.Minute {
		t.Fatalf("critical repeat = %v, want the 240m default", got)
	}
}

func TestThrottledEvents_OnlyFindingAlerts(t *testing.T) {
	for _, typ := range []string{"finding_critical", "finding_opened", "finding_resolved"} {
		if !Throttled(typ) {
			t.Errorf("%s is not throttled", typ)
		}
	}
	for _, typ := range []string{"incident_escalated", "approval_needed", "action_failed", ""} {
		if Throttled(typ) {
			t.Errorf("%s is throttled; it must pass through", typ)
		}
	}
}

func TestAlertPolicyString_NamesEverySetting(t *testing.T) {
	got := testPolicy(t, "22:00", "06:30").String()
	want := "cooldown critical=15m0s warning=30m0s info=6h0m0s; repeat critical=4h0m0s " +
		"warning=1h0m0s info=0s; quiet 22:00-06:30 UTC"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
