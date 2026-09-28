package policy

import (
	"context"
	"testing"
	"time"
)

// Regression tests for G4-B02, G4-B03 and G4-B15: the standing gate must
// honor the configured trust ramp, tier3 flags and configured maintenance
// window, must never execute backend signals without approval, and a
// deadline override may lift only the window restriction.

var safetyNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func enabledRuntime() RuntimeState {
	return RuntimeState{
		ExecutorEnabled: true, TrustLevel: TrustAutonomous,
		ExecutionMode: ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
		RampStart: safetyNow.Add(-60 * 24 * time.Hour), InConfiguredWindow: true,
	}
}

func moderateRequest() ActionRequest {
	req := validIndexRequest()
	req.Contract.RiskTier = RiskModerate
	return req
}

func TestGateRequiresTierFlagsAndRamp(t *testing.T) {
	tests := []struct {
		name string
		risk RiskTier
		edit func(*RuntimeState)
	}{
		{"safe without tier3_safe", RiskSafe, func(s *RuntimeState) { s.Tier3Safe = false }},
		{"safe ramp under 8 days", RiskSafe, func(s *RuntimeState) {
			s.RampStart = safetyNow.Add(-7 * 24 * time.Hour)
		}},
		{"safe with unknown ramp start", RiskSafe, func(s *RuntimeState) {
			s.RampStart = time.Time{}
		}},
		{"moderate without tier3_moderate", RiskModerate, func(s *RuntimeState) {
			s.Tier3Moderate = false
		}},
		{"moderate ramp at 30 days", RiskModerate, func(s *RuntimeState) {
			s.RampStart = safetyNow.Add(-30 * 24 * time.Hour)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := enabledRuntime()
			tt.edit(&runtime)
			gate := newTestGate(t, gateFixture{
				runtime: runtime, runtimeSet: true, now: safetyNow,
			})
			req := validIndexRequest()
			req.Contract.RiskTier = tt.risk

			decision := gate.Authorize(context.Background(), req)

			assertDecision(t, decision, VerdictBlocked, ReasonTrustRampNotSatisfied)
		})
	}
}

func TestGateExecutesWhenRampFlagsAndWindowsAreSatisfied(t *testing.T) {
	for _, risk := range []RiskTier{RiskSafe, RiskModerate} {
		gate := newTestGate(t, gateFixture{
			runtime: enabledRuntime(), runtimeSet: true, now: safetyNow,
		})
		req := validIndexRequest()
		req.Contract.RiskTier = risk

		decision := gate.Authorize(context.Background(), req)

		assertDecision(t, decision, VerdictExecute, ReasonAuthorized)
	}
}

func TestGateReadOnlyIgnoresRamp(t *testing.T) {
	runtime := enabledRuntime()
	runtime.Tier3Safe, runtime.RampStart = false, time.Time{}
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow})
	req := validIndexRequest()
	req.Contract.RiskTier = RiskReadOnly

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictExecute, ReasonAuthorized)
}

func TestGateModerateHonorsConfiguredWindow(t *testing.T) {
	runtime := enabledRuntime()
	runtime.InConfiguredWindow = false
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow})

	decision := gate.Authorize(context.Background(), moderateRequest())

	assertDecision(t, decision, VerdictBlocked, ReasonOutsideMaintenanceWindow)
}

func TestGateAdvisoryModerateQueuesWithoutRamp(t *testing.T) {
	runtime := enabledRuntime()
	runtime.TrustLevel, runtime.Tier3Moderate = TrustAdvisory, false
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow})

	decision := gate.Authorize(context.Background(), moderateRequest())

	assertDecision(t, decision, VerdictQueueApproval, ReasonApprovalRequired)
}

func TestGateBackendSignalsAlwaysQueue(t *testing.T) {
	for _, actionType := range []string{"cancel_backend", "terminate_backend"} {
		t.Run(actionType, func(t *testing.T) {
			gate := newTestGate(t, gateFixture{
				runtime: enabledRuntime(), runtimeSet: true, now: safetyNow,
			})
			req := validIndexRequest()
			req.Contract = &ActionContract{ActionType: actionType, RiskTier: RiskSafe}

			decision := gate.Authorize(context.Background(), req)

			assertDecision(t, decision, VerdictQueueApproval, ReasonApprovalRequired)
		})
	}
}

func TestDeadlineOverrideCannotBypassTrustModeOrRisk(t *testing.T) {
	tests := []struct {
		name string
		edit func(*RuntimeState, *ActionRequest)
		want Verdict
	}{
		{"approval mode", func(s *RuntimeState, _ *ActionRequest) {
			s.ExecutionMode = ExecutionApproval
		}, VerdictQueueApproval},
		{"advisory trust", func(s *RuntimeState, _ *ActionRequest) {
			s.TrustLevel = TrustAdvisory
		}, VerdictQueueApproval},
		{"high risk", func(_ *RuntimeState, r *ActionRequest) {
			r.Contract.RiskTier = RiskHigh
		}, VerdictQueueApproval},
		{"ramp not satisfied", func(s *RuntimeState, _ *ActionRequest) {
			s.Tier3Moderate = false
		}, VerdictBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := UnattendedProfile()
			doc.MaintenanceWindows = []string{"0 2 * * 0"}
			runtime := enabledRuntime()
			req := moderateRequest()
			req.Feature = "freeze"
			req.Deadline = &DeadlineContext{
				Kind: DeadlineXID, Urgency: UrgencyCritical,
				HardAt: safetyNow.Add(time.Hour),
			}
			tt.edit(&runtime, &req)
			gate := newTestGate(t, gateFixture{
				runtime: runtime, runtimeSet: true, policy: doc, now: safetyNow,
			})

			decision := gate.Authorize(context.Background(), req)

			if decision.Verdict != tt.want || decision.OffWindowOK {
				t.Fatalf("decision = %#v, want verdict %q without override", decision, tt.want)
			}
		})
	}
}

func TestDeadlineOverrideLiftsConfiguredWindowOnly(t *testing.T) {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	runtime := enabledRuntime()
	runtime.InConfiguredWindow = false
	req := moderateRequest()
	req.Feature = "freeze"
	req.Deadline = &DeadlineContext{
		Kind: DeadlineXID, Urgency: UrgencyCritical, HardAt: safetyNow.Add(time.Hour),
	}
	gate := newTestGate(t, gateFixture{
		runtime: runtime, runtimeSet: true, policy: doc, now: safetyNow,
	})

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictExecute, ReasonDeadlineOverride)
	if !decision.OffWindowOK {
		t.Fatal("OffWindowOK = false, want true")
	}
}

func TestRetentionDeleteIsTrustedInternalControlWithoutSQL(t *testing.T) {
	gate := newTestGate(t, gateFixture{
		runtime: enabledRuntime(), runtimeSet: true, now: safetyNow,
	})
	req := ActionRequest{
		Contract: &ActionContract{
			ActionType: "retention_delete", RiskTier: RiskModerate,
		},
		InternalControl: true, Feature: string(ChangeRetention),
		TargetObjs: []string{"public.events"},
	}

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictExecute, ReasonAuthorized)
}
