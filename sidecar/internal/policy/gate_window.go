package policy

import (
	"context"
	"fmt"
	"time"
)

// WindowStart is when an operator-approved request may next start: the
// window opening (or now, inside an open window), when that window
// closes (zero: not within the scan) and the zone it is written in.
type WindowStart struct {
	At       time.Time
	ClosesAt time.Time
	Location *time.Location
}

// WindowPlanner plans the next start of an operator-approved request that
// the maintenance window alone withholds (policy-04).
type WindowPlanner interface {
	NextWindowStart(context.Context, ActionRequest) (WindowStart, error)
}

// windowVerdict is the window's verdict on a moderate or high request:
// none inside an open window with room for the start margin, else a valid
// deadline override, else blocked as closing (inside) or outside.
func (gate *authorizationGate) windowVerdict(
	doc Document, req ActionRequest, inWindow, closing bool,
) (Decision, bool) {
	if inWindow && !closing {
		return Decision{}, false
	}
	now := gate.now()
	if validDeadlineOverride(doc, req.Deadline, now) {
		decision := gate.decision(req, VerdictExecute, ReasonDeadlineOverride)
		decision.OffWindowOK = true
		return decision, true
	}
	if inWindow {
		return gate.decision(req, VerdictBlocked, ReasonWindowClosing), true
	}
	return gate.decision(req, VerdictBlocked, ReasonOutsideMaintenanceWindow), true
}

// windowClosing reports an open window that closes before the policy's
// window_start_margin_minutes elapses. trustBinds says whether
// trust.maintenance_window is part of the window for this request; a
// runtime that does not carry its expression leaves only the policy
// windows to measure. A window that cannot be built fails closed.
func (gate *authorizationGate) windowClosing(
	doc Document, runtime RuntimeState, trustBinds bool, now time.Time,
) bool {
	margin := time.Duration(doc.WindowStartMarginMinutes) * time.Minute
	if margin <= 0 {
		return false
	}
	window, err := NewEffectiveWindow(trustBinds && runtime.TrustWindow != "",
		runtime.TrustWindow, doc.MaintenanceWindows)
	if err != nil {
		return true
	}
	closes, ok := window.ClosesAt(now)
	return ok && closes.Sub(now) < margin
}

// windowOnly reports an operator approval the window alone withholds: it
// re-runs the evaluation with the window treated as open, so every other
// gate (stops, trust, class allowlist, facts, verification) still binds.
func (gate *authorizationGate) windowOnly(
	ctx context.Context, req ActionRequest, decision Decision,
) bool {
	if !req.OperatorApproved || decision.Verdict == VerdictExecute {
		return false
	}
	if decision.Reason != ReasonOutsideMaintenanceWindow &&
		decision.Reason != ReasonWindowClosing {
		return false
	}
	req.assumeWindowOpen = true
	return gate.evaluate(ctx, req).Verdict == VerdictExecute
}

// NextWindowStart is the first instant at or after now at which the
// operator gate's window (trust.maintenance_window when configured, and a
// policy window) is open with the start margin to spare. It fails when
// the runtime or policy cannot be read, and with ErrNoWindowWithinScan
// when no such start exists within 8 days.
func (gate *authorizationGate) NextWindowStart(
	ctx context.Context, req ActionRequest,
) (WindowStart, error) {
	runtime, err := gate.runtime(ctx, req)
	if err != nil {
		return WindowStart{}, fmt.Errorf("read runtime state: %w", err)
	}
	doc, err := gate.policy(ctx, req)
	if err != nil {
		return WindowStart{}, fmt.Errorf("read standing policy: %w", err)
	}
	if err := ValidateDocument(doc); err != nil {
		return WindowStart{}, fmt.Errorf("standing policy is invalid: %w", err)
	}
	window, err := NewEffectiveWindow(runtime.WindowConfigured, runtime.TrustWindow,
		doc.MaintenanceWindows)
	if err != nil {
		return WindowStart{}, err
	}
	margin := time.Duration(doc.WindowStartMarginMinutes) * time.Minute
	at, closes, ok := window.NextStart(gate.now(), margin)
	if !ok {
		return WindowStart{}, ErrNoWindowWithinScan
	}
	return WindowStart{At: at, ClosesAt: closes, Location: window.Location()}, nil
}
