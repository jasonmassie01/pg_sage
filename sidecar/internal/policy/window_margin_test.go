package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// policy-04: window_start_margin_minutes, the window_closing reason, the
// Explain WindowOnly flag and the gate's NextWindowStart planner.
//
// No concurrent access tests: the margin and the planner read the same
// immutable runtime and document snapshot as the rest of the gate, which
// gate_test.go already exercises concurrently.

// marginDoc is the staffed profile with a zoned window and a margin.
func marginDoc(margin int64) Document {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00 UTC"}
	doc.WindowStartMarginMinutes = margin
	return doc
}

// wednesday is 2026-11-11 (a Wednesday) at hour:minute UTC.
func wednesday(hour, minute int) time.Time {
	return time.Date(2026, 11, 11, hour, minute, 0, 0, time.UTC)
}

func marginGate(t *testing.T, doc Document, runtime RuntimeState, now time.Time) Gate {
	t.Helper()
	return newTestGate(t, gateFixture{policy: doc, runtime: runtime, runtimeSet: true, now: now})
}

func TestWindowMarginOperatorBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    time.Time
		margin int64
		want   Reason
	}{
		{"exactly the margin left starts", wednesday(4, 30), 30, ReasonOperatorApproved},
		{"one minute over the margin starts", wednesday(4, 29), 30, ReasonOperatorApproved},
		{"one minute under the margin waits", wednesday(4, 31), 30, ReasonWindowClosing},
		{"no margin starts in the last minute", wednesday(4, 59), 0, ReasonOperatorApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := marginGate(t, marginDoc(tc.margin), newTestGateRuntime(), tc.now)
			got := gate.Authorize(context.Background(), operatorRequest(RiskModerate))
			verdict := VerdictExecute
			if tc.want == ReasonWindowClosing {
				verdict = VerdictBlocked
			}
			assertDecision(t, got, verdict, tc.want)
		})
	}
}

func TestWindowMarginBindsSelfInitiatedAndHigh(t *testing.T) {
	gate := marginGate(t, marginDoc(30), newTestGateRuntime(), wednesday(4, 45))
	req := validIndexRequest()
	req.Contract.RiskTier = RiskModerate
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, ReasonWindowClosing)
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskHigh)),
		VerdictBlocked, ReasonWindowClosing)
}

func TestWindowMarginDoesNotBindSafeActions(t *testing.T) {
	gate := marginGate(t, marginDoc(30), newTestGateRuntime(), wednesday(4, 59))
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskSafe)),
		VerdictExecute, ReasonOperatorApproved)
}

func TestWindowMarginUsesTheTrustWindowClose(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.WindowConfigured, runtime.InConfiguredWindow = true, true
	runtime.TrustWindow = "daily 01:00-04:00 UTC"
	gate := marginGate(t, marginDoc(30), runtime, wednesday(3, 45))
	// The policy window is open until 05:00, but the trust window closes at
	// 04:00: 15 minutes left is under the 30 minute margin.
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskModerate)),
		VerdictBlocked, ReasonWindowClosing)
}

func TestWindowMarginKeepsMitigationWindowExempt(t *testing.T) {
	runtime := advisoryRuntime()
	runtime.InConfiguredWindow = true
	gate := mitigationGate(t, marginDoc(720), runtime)
	assertDecision(t, gate.Authorize(context.Background(),
		cancelRequest(RollbackMitigationOnly, true)), VerdictExecute, ReasonOperatorApproved)
}

func TestExplainWindowOnlyOutsideTheWindow(t *testing.T) {
	gate := marginGate(t, marginDoc(0), newTestGateRuntime(), wednesday(14, 0))
	got := mustExplainer(t, gate).Explain(context.Background(), operatorRequest(RiskModerate))
	if got.Reason != ReasonOutsideMaintenanceWindow || !got.WindowOnly {
		t.Fatalf("Explain = %#v; want outside_maintenance_window with WindowOnly", got)
	}
	authorized := gate.Authorize(context.Background(), operatorRequest(RiskModerate))
	if authorized.Verdict != VerdictBlocked || authorized.WindowOnly {
		t.Fatalf("Authorize = %#v; want blocked and never WindowOnly", authorized)
	}
}

func TestExplainWindowOnlyForWindowClosing(t *testing.T) {
	gate := marginGate(t, marginDoc(30), newTestGateRuntime(), wednesday(4, 45))
	got := mustExplainer(t, gate).Explain(context.Background(), operatorRequest(RiskModerate))
	if got.Reason != ReasonWindowClosing || !got.WindowOnly {
		t.Fatalf("Explain = %#v; want window_closing with WindowOnly", got)
	}
}

func TestExplainWindowOnlyIsFalseForOtherRefusals(t *testing.T) {
	stopped := newTestGateRuntime()
	stopped.EmergencyStop = true
	narrowed := marginDoc(0)
	narrowed.AllowedChangeClasses = []ChangeClass{ChangeAnalyze}
	observe := newTestGateRuntime()
	observe.TrustLevel = TrustObservation
	for name, gate := range map[string]Gate{
		"emergency stop": marginGate(t, marginDoc(0), stopped, wednesday(14, 0)),
		"class removed":  marginGate(t, narrowed, newTestGateRuntime(), wednesday(14, 0)),
		"observation":    marginGate(t, marginDoc(0), observe, wednesday(14, 0)),
		"inside window":  marginGate(t, marginDoc(0), newTestGateRuntime(), wednesday(2, 0)),
	} {
		got := mustExplainer(t, gate).Explain(context.Background(),
			operatorRequest(RiskModerate))
		if got.WindowOnly {
			t.Fatalf("%s: Explain = %#v; WindowOnly must be false", name, got)
		}
	}
}

func TestExplainWindowOnlyNeedsAnOperatorApproval(t *testing.T) {
	gate := marginGate(t, marginDoc(0), newTestGateRuntime(), wednesday(14, 0))
	req := validIndexRequest()
	req.Contract.RiskTier = RiskModerate
	got := mustExplainer(t, gate).Explain(context.Background(), req)
	if got.Reason != ReasonOutsideMaintenanceWindow || got.WindowOnly {
		t.Fatalf("Explain = %#v; a self-initiated request is never WindowOnly", got)
	}
}

func mustPlanner(t *testing.T, gate Gate) WindowPlanner {
	t.Helper()
	planner, ok := gate.(WindowPlanner)
	if !ok {
		t.Fatalf("%T does not implement policy.WindowPlanner", gate)
	}
	return planner
}

func TestNextWindowStartFromOutsideTheWindow(t *testing.T) {
	gate := marginGate(t, marginDoc(0), newTestGateRuntime(), wednesday(14, 0))
	start, err := mustPlanner(t, gate).NextWindowStart(context.Background(),
		operatorRequest(RiskModerate))
	if err != nil {
		t.Fatalf("NextWindowStart: %v", err)
	}
	wantAt := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	wantClose := time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC)
	if !start.At.Equal(wantAt) || !start.ClosesAt.Equal(wantClose) {
		t.Fatalf("start = %+v; want %s until %s", start, wantAt, wantClose)
	}
	if start.Location == nil || start.Location.String() != "UTC" {
		t.Fatalf("start.Location = %v, want UTC", start.Location)
	}
}

func TestNextWindowStartSkipsAClosingWindow(t *testing.T) {
	gate := marginGate(t, marginDoc(30), newTestGateRuntime(), wednesday(4, 45))
	start, err := mustPlanner(t, gate).NextWindowStart(context.Background(),
		operatorRequest(RiskModerate))
	if err != nil {
		t.Fatalf("NextWindowStart: %v", err)
	}
	if want := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC); !start.At.Equal(want) {
		t.Fatalf("start.At = %s, want %s (tonight's window is closing)", start.At, want)
	}
}

func TestNextWindowStartHonoursTheTrustWindow(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.WindowConfigured, runtime.InConfiguredWindow = true, false
	runtime.TrustWindow = "weekends UTC"
	doc := marginDoc(0)
	doc.MaintenanceWindows = []string{"always"}
	gate := marginGate(t, doc, runtime, wednesday(14, 0))
	start, err := mustPlanner(t, gate).NextWindowStart(context.Background(),
		operatorRequest(RiskModerate))
	if err != nil {
		t.Fatalf("NextWindowStart: %v", err)
	}
	if want := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC); !start.At.Equal(want) {
		t.Fatalf("start.At = %s, want Saturday %s", start.At, want)
	}
}

func TestNextWindowStartNoneWithinTheScan(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.WindowConfigured, runtime.InConfiguredWindow = true, false
	runtime.TrustWindow = "weekends UTC"
	doc := marginDoc(0)
	gate := marginGate(t, doc, runtime, wednesday(14, 0))
	_, err := mustPlanner(t, gate).NextWindowStart(context.Background(),
		operatorRequest(RiskModerate))
	if !errors.Is(err, ErrNoWindowWithinScan) {
		t.Fatalf("NextWindowStart error = %v, want ErrNoWindowWithinScan", err)
	}
}

func TestNextWindowStartPolicyUnavailable(t *testing.T) {
	gate := newTestGate(t, gateFixture{policy: marginDoc(0),
		policyErr: errors.New("connection refused"), now: wednesday(14, 0)})
	_, err := mustPlanner(t, gate).NextWindowStart(context.Background(),
		operatorRequest(RiskModerate))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("NextWindowStart error = %v, want a policy read error", err)
	}
}
