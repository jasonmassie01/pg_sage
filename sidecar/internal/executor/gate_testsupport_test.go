package executor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// withTestStandingGate installs a real standing gate over the executor's
// own runtime state (trust, mode, tier flags, ramp, emergency stop), as
// production does, with an in-memory unattended policy whose windows are
// always open. Tests that drive RunCycle need a gate: without one the
// executor fails closed (G4-I01).
func withTestStandingGate(e *Executor) *Executor {
	return withTestStandingGateAt(e, time.Time{})
}

// withTestStandingGateAt pins the gate clock (and the configured-window
// check) to now, for tests that evaluate policy at a fixed time. A zero
// now uses the wall clock.
func withTestStandingGateAt(e *Executor, now time.Time) *Executor {
	if e.pool == nil {
		// No database to read the emergency-stop flag from.
		e.emergencyStopFn = func(context.Context) bool { return false }
	}
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	var clock func() time.Time
	if !now.IsZero() {
		clock = func() time.Time { return now }
	}
	e.EnableStandingPolicyDocument(doc, clock)
	return e
}
