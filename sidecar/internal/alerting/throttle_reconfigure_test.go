package alerting

import (
	"testing"
	"time"
)

// alerting-06: the alerting owner reconfigures a running manager's
// throttle; the sent records survive, so an open alert is not re-sent.
// No concurrent access tests beyond the mutex: Reconfigure takes t.mu
// like every other method.

func TestThrottleReconfigure_ChangesCooldownAndQuietHoursKeepsRecords(t *testing.T) {
	th := NewThrottle(15, "", "", "UTC")
	now := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	th.now = func() time.Time { return now }
	th.Record("bloat:t", "warning")
	if got := th.cooldownFor("warning"); got != 30*time.Minute {
		t.Fatalf("warning cooldown = %v, want the 30m floor", got)
	}
	warns := th.Reconfigure(120, map[string]int{"warning": 0}, "22:00", "06:00", "UTC")
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if got := th.cooldownFor("warning"); got != 120*time.Minute {
		t.Fatalf("warning cooldown = %v, want 120m", got)
	}
	if !th.IsQuietHours(now) {
		t.Fatal("quiet hours not applied")
	}
	if th.repeatFor("warning") != 0 {
		t.Fatalf("warning repeat = %v, want never", th.repeatFor("warning"))
	}
	if d := th.Decide("bloat:t", "warning"); d != DecisionThrottled {
		t.Fatalf("decision after reconfigure = %v, want the record kept (throttled)", d)
	}
}

func TestThrottleReconfigure_InvalidValuesWarnAndDisable(t *testing.T) {
	th := NewThrottle(15, "22:00", "06:00", "UTC")
	warns := th.Reconfigure(15, nil, "25:00", "06:00", "Nowhere/City")
	if len(warns) != 2 {
		t.Fatalf("warnings = %v, want timezone and quiet hours", warns)
	}
	if th.IsQuietHours(time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)) {
		t.Fatal("invalid quiet hours left the old window active")
	}
}
