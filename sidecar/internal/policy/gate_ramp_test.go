package policy

import (
	"context"
	"testing"
	"time"
)

// Fast elevation: the trust ramp ages come from trust.ramp_safe_hours and
// trust.ramp_moderate_hours through RuntimeState. A zero age is the spec
// ramp (8 / 31 days), a configured ramp never drops below MinRampAge, and
// an action that cannot be rolled back never benefits from a shortened
// ramp: it always waits at least the spec ramp.

func rampRequest(tier RiskTier, rollback RollbackClass) ActionRequest {
	req := validIndexRequest()
	req.Contract.RiskTier = tier
	req.Contract.RollbackClass = rollback
	return req
}

func rampDecision(t *testing.T, runtime RuntimeState, req ActionRequest) Decision {
	t.Helper()
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow})
	return gate.Authorize(context.Background(), req)
}

func rampRuntime(age, safe, moderate time.Duration) RuntimeState {
	runtime := enabledRuntime()
	runtime.RampStart = safetyNow.Add(-age)
	runtime.SafeRampAge, runtime.ModerateRampAge = safe, moderate
	return runtime
}

func TestSpecRampConstants(t *testing.T) {
	if SpecSafeRampAge != 8*24*time.Hour || SpecModerateRampAge != 31*24*time.Hour ||
		MinRampAge != time.Hour {
		t.Fatalf("spec ramp = %s / %s, minimum %s", SpecSafeRampAge, SpecModerateRampAge,
			MinRampAge)
	}
}

func TestConfiguredRampIsHonouredAtItsBoundary(t *testing.T) {
	cases := []struct {
		name    string
		tier    RiskTier
		age     time.Duration
		execute bool
	}{
		{"safe one minute early", RiskSafe, 59 * time.Minute, false},
		{"safe at the ramp", RiskSafe, time.Hour, true},
		{"moderate one minute early", RiskModerate, 4*time.Hour - time.Minute, false},
		{"moderate at the ramp", RiskModerate, 4 * time.Hour, true},
	}
	for _, c := range cases {
		for _, rollback := range []RollbackClass{RollbackReversible, RollbackNoRollbackNeeded} {
			d := rampDecision(t, rampRuntime(c.age, time.Hour, 4*time.Hour),
				rampRequest(c.tier, rollback))
			if c.execute {
				assertDecision(t, d, VerdictExecute, ReasonAuthorized)
			} else {
				assertDecision(t, d, VerdictBlocked, ReasonTrustRampNotSatisfied)
			}
		}
	}
}

// Default-value masking: a runtime that does not carry the configured
// ramp (zero) or carries a nonsensical one (negative) gets the spec ramp.
func TestZeroOrNegativeRampTakesTheSpecRamp(t *testing.T) {
	for _, configured := range []time.Duration{0, -time.Hour} {
		for _, c := range []struct {
			tier RiskTier
			spec time.Duration
		}{{RiskSafe, SpecSafeRampAge}, {RiskModerate, SpecModerateRampAge}} {
			req := rampRequest(c.tier, RollbackReversible)
			early := rampDecision(t, rampRuntime(c.spec-time.Minute, configured, configured), req)
			assertDecision(t, early, VerdictBlocked, ReasonTrustRampNotSatisfied)
			ready := rampDecision(t, rampRuntime(c.spec, configured, configured), req)
			assertDecision(t, ready, VerdictExecute, ReasonAuthorized)
		}
	}
}

// A ramp below the hour (only possible programmatically) is clamped.
func TestSubHourRampIsClampedToTheMinimum(t *testing.T) {
	runtime := rampRuntime(45*time.Minute, time.Minute, 30*time.Minute)
	for _, tier := range []RiskTier{RiskSafe, RiskModerate} {
		d := rampDecision(t, runtime, rampRequest(tier, RollbackReversible))
		assertDecision(t, d, VerdictBlocked, ReasonTrustRampNotSatisfied)
	}
	runtime = rampRuntime(MinRampAge, time.Minute, 30*time.Minute)
	for _, tier := range []RiskTier{RiskSafe, RiskModerate} {
		d := rampDecision(t, runtime, rampRequest(tier, RollbackReversible))
		assertDecision(t, d, VerdictExecute, ReasonAuthorized)
	}
}

var irreversibleRollbacks = []RollbackClass{RollbackNotReversible, RollbackForwardFixOnly,
	RollbackApplication, RollbackNotApplicable, RollbackMitigationOnly, "",
	RollbackClass("reversible-ish-maybe")}

// Invariant: an action that cannot be rolled back keeps the spec ramp
// under the fastest configuration.
func TestIrreversibleActionsKeepTheSpecRamp(t *testing.T) {
	for _, rollback := range irreversibleRollbacks {
		for _, c := range []struct {
			tier RiskTier
			spec time.Duration
		}{{RiskSafe, SpecSafeRampAge}, {RiskModerate, SpecModerateRampAge}} {
			req := rampRequest(c.tier, rollback)
			fast := rampDecision(t, rampRuntime(c.spec-time.Minute, time.Hour, time.Hour), req)
			if fast.Verdict != VerdictBlocked || fast.Reason != ReasonTrustRampNotSatisfied {
				t.Fatalf("%s %q before the spec ramp under a 1h ramp: %s/%s", c.tier,
					rollback, fast.Verdict, fast.Reason)
			}
			spec := rampDecision(t, rampRuntime(c.spec, time.Hour, time.Hour), req)
			if spec.Reason == ReasonTrustRampNotSatisfied {
				t.Fatalf("%s %q still ramp-blocked after the spec ramp", c.tier, rollback)
			}
		}
	}
}

// A ramp longer than the spec slows every action, reversible or not.
func TestLongerRampAppliesToEveryAction(t *testing.T) {
	long := 10 * 24 * time.Hour
	for _, rollback := range append([]RollbackClass{RollbackReversible}, irreversibleRollbacks...) {
		req := rampRequest(RiskSafe, rollback)
		d := rampDecision(t, rampRuntime(9*24*time.Hour, long, 40*24*time.Hour), req)
		if d.Reason != ReasonTrustRampNotSatisfied {
			t.Fatalf("%q at 9 days under a 10-day ramp: %s/%s", rollback, d.Verdict, d.Reason)
		}
		d = rampDecision(t, rampRuntime(long, long, 40*24*time.Hour), req)
		if d.Reason == ReasonTrustRampNotSatisfied {
			t.Fatalf("%q still ramp-blocked at 10 days", rollback)
		}
	}
}

// The ramp only gates what trust allows: a fast ramp never widens trust,
// execution mode, tier flags, an unknown ramp start or the emergency stop.
func TestFastRampNeverWidensTheOperatorBound(t *testing.T) {
	cases := map[string]struct {
		edit    func(*RuntimeState)
		verdict Verdict
		reason  Reason
	}{
		"emergency stop": {func(s *RuntimeState) { s.EmergencyStop = true },
			VerdictBlocked, ReasonEmergencyStop},
		"observation": {func(s *RuntimeState) { s.TrustLevel = TrustObservation },
			VerdictObserveOnly, ReasonObserveOnly},
		"approval mode": {func(s *RuntimeState) { s.ExecutionMode = ExecutionApproval },
			VerdictQueueApproval, ReasonApprovalRequired},
		"no tier3_moderate": {func(s *RuntimeState) { s.Tier3Moderate = false },
			VerdictBlocked, ReasonTrustRampNotSatisfied},
		"unknown ramp start": {func(s *RuntimeState) { s.RampStart = time.Time{} },
			VerdictBlocked, ReasonTrustRampNotSatisfied},
		"advisory moderate": {func(s *RuntimeState) { s.TrustLevel = TrustAdvisory },
			VerdictQueueApproval, ReasonApprovalRequired},
	}
	for name, c := range cases {
		runtime := rampRuntime(2*time.Hour, time.Hour, time.Hour)
		c.edit(&runtime)
		d := rampDecision(t, runtime, rampRequest(RiskModerate, RollbackReversible))
		if d.Verdict != c.verdict || d.Reason != c.reason {
			t.Errorf("%s: %s/%s, want %s/%s", name, d.Verdict, d.Reason, c.verdict, c.reason)
		}
	}
	high := rampDecision(t, rampRuntime(2*time.Hour, time.Hour, time.Hour),
		rampRequest(RiskHigh, RollbackReversible))
	if high.Verdict == VerdictExecute {
		t.Fatalf("a high-risk action executed under a fast ramp: %s", high.Reason)
	}
}

func TestFastRampExplainMatchesAuthorize(t *testing.T) {
	for _, age := range []time.Duration{30 * time.Minute, 2 * time.Hour} {
		runtime := rampRuntime(age, time.Hour, time.Hour)
		gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow})
		req := rampRequest(RiskSafe, RollbackReversible)
		explained := mustExplainer(t, gate).Explain(context.Background(), req)
		authorized := gate.Authorize(context.Background(), req)
		if explained.Verdict != authorized.Verdict || explained.Reason != authorized.Reason {
			t.Fatalf("age %s: explain %s/%s, authorize %s/%s", age, explained.Verdict,
				explained.Reason, authorized.Verdict, authorized.Reason)
		}
	}
}

// fastestProfile is the fastest configuration the config allows: both
// ramps at the one-hour minimum, the ramp started 90 minutes ago.
func fastestProfile(f *autonomyFixture) {
	f.runtime.RampStart = f.now.Add(-90 * time.Minute)
	f.runtime.SafeRampAge, f.runtime.ModerateRampAge = MinRampAge, MinRampAge
}

// The M7 no-premature-autonomy matrix under the fastest profile, with the
// emergency stop as an extra dimension: fast elevation changes when trust
// is earned, never what a level, the reversibility cap, the window, the
// operator bound or the emergency stop allow.
func TestAutonomyGateNeverExecutesPrematurelyFastProfile(t *testing.T) {
	executed, total := runPrematureMatrix(t, fastestProfile, []bool{false, true})
	if executed == 0 || total != 2*18144 {
		t.Fatalf("matrix: %d combinations, %d executed", total, executed)
	}
}
