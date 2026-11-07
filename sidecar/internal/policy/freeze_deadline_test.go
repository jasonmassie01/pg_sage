package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-03: a table past autovacuum_freeze_max_age (the most urgent
// case) keeps a hard deadline ahead, so the documented xid deadline
// override and the budget bypass fire for it. No concurrency test: both
// checks are pure functions of their arguments.

func pastMaximumDeadline(t *testing.T, now time.Time) *DeadlineContext {
	t.Helper()
	assessment, err := freeze.EvaluateHorizon(now, freeze.HorizonSample{
		Database: "orders", Schema: "public", Table: "events",
		XIDAge: 200_000_001, XIDMaxAge: 200_000_000, XIDsPerSecond: 50,
		MultiXactAge: 5, MultiXactMaxAge: 400_000_000, MultiXactsPerSecond: 0.001,
	}, freeze.BufferThresholds(25))
	if err != nil {
		t.Fatalf("EvaluateHorizon: %v", err)
	}
	if assessment.Urgency != freeze.UrgencyRed {
		t.Fatalf("urgency = %q, want red past the freeze maximum", assessment.Urgency)
	}
	return &DeadlineContext{Kind: DeadlineXID, Urgency: UrgencyCritical,
		HardAt: assessment.Deadline.HardAt}
}

func TestPastFreezeMaximumDeadlineKeepsTheOverride(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	deadline := pastMaximumDeadline(t, now)
	if !deadline.HardAt.After(now) {
		t.Fatalf("HardAt = %s, want after %s", deadline.HardAt, now)
	}
	allowed := Document{DeadlineOverrides: map[DeadlineKind]bool{DeadlineXID: true}}
	if !validDeadlineOverride(allowed, deadline, now) {
		t.Fatal("xid deadline override refused for a table past the freeze maximum")
	}
	if validDeadlineOverride(Document{}, deadline, now) {
		t.Fatal("override granted without a deadline_overrides document entry")
	}
}

func TestPastFreezeMaximumDeadlineBypassesTheBudget(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	deadline := pastMaximumDeadline(t, now)
	got := BudgetBypassFor(freezeRequest(deadline), now)
	want := "xid runway critical, wraparound hard deadline " +
		deadline.HardAt.UTC().Format(time.RFC3339)
	if got != want || !strings.HasPrefix(got, "xid runway critical") {
		t.Fatalf("BudgetBypassFor = %q, want %q", got, want)
	}
}

// Past the wraparound warn limit the custodian's proposals are routed one
// after another, each behind the previous table's VACUUM: the deadline a
// scan computed must still be ahead when a later table reaches the gate,
// so the override and the bypass still fire an hour after the scan.
func TestPastWarnLimitDeadlineOutlivesTheScan(t *testing.T) {
	scanned := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	assessment, err := freeze.EvaluateHorizon(scanned, freeze.HorizonSample{
		Database: "orders", Schema: "public", Table: "events",
		XIDAge: freeze.WraparoundWarnAge + 1, XIDMaxAge: 200_000_000, XIDsPerSecond: 50,
		MultiXactAge: 5, MultiXactMaxAge: 400_000_000, MultiXactsPerSecond: 0.001,
	}, freeze.BufferThresholds(25))
	if err != nil {
		t.Fatalf("EvaluateHorizon: %v", err)
	}
	deadline := &DeadlineContext{Kind: DeadlineXID, Urgency: UrgencyCritical,
		HardAt: assessment.Deadline.HardAt}
	gated := scanned.Add(time.Hour)
	allowed := Document{DeadlineOverrides: map[DeadlineKind]bool{DeadlineXID: true}}
	if !validDeadlineOverride(allowed, deadline, gated) {
		t.Fatalf("override refused an hour after the scan; HardAt = %s", deadline.HardAt)
	}
	if got := BudgetBypassFor(freezeRequest(deadline), gated); !strings.HasPrefix(got,
		"xid runway critical") {
		t.Fatalf("BudgetBypassFor an hour after the scan = %q, want xid runway critical", got)
	}
}
