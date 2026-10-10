package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Narrowing contracts (AGENTDB-SPEC §6.2.4): guard_revoke, guard_freeze,
// the kill steps, guard_watchdog_cancel and estate_quarantine only take
// access away. They pass A1's stops (executor disabled, emergency stop,
// replica), A5's trust ceiling and A6's document, budgets and windows, but
// are still validated (A2), provider- and fact-checked (A3) and recorded.

func narrowingRequest() ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "guard_freeze", RiskTier: RiskSafe,
			RollbackClass: RollbackReversible, Narrowing: true},
		InternalControl: true,
		TargetObjs:      []string{"principal:agp_aaaaaaaaaaaaaaaaaaaa"},
		Feature:         string(ChangeAgentAccess),
	}
}

func TestNarrowingExecutesWithoutStops(t *testing.T) {
	var calls []string
	gate := newTestGate(t, gateFixture{calls: &calls})
	got := gate.Authorize(context.Background(), narrowingRequest())
	assertDecision(t, got, VerdictExecute, ReasonNarrowing)
	if got.RiskTier != RiskSafe {
		t.Fatalf("RiskTier = %q, want safe", got.RiskTier)
	}
	// No budget, usage or policy-document read: narrowing never spends.
	assertNotCalled(t, calls, "usage", "policy", "window")
	assertCallPrefix(t, calls, []string{"runtime"})
	if calls[len(calls)-1] != "evidence" {
		t.Fatalf("calls = %#v, want the decision recorded last", calls)
	}
}

func TestNarrowingPassesEveryHardStop(t *testing.T) {
	cases := map[string]func(*RuntimeState){
		"emergency_stop":    func(r *RuntimeState) { r.EmergencyStop = true },
		"executor_disabled": func(r *RuntimeState) { r.ExecutorEnabled = false },
		"replica":           func(r *RuntimeState) { r.IsReplica = true },
		"all three": func(r *RuntimeState) {
			r.EmergencyStop, r.ExecutorEnabled, r.IsReplica = true, false, true
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			runtime := newTestGateRuntime()
			mutate(&runtime)
			gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
			got := gate.Authorize(context.Background(), narrowingRequest())
			assertDecision(t, got, VerdictExecute, ReasonNarrowingDuringStop)
		})
	}
}

func TestNarrowingRequestFlaggedReplicaPasses(t *testing.T) {
	req := narrowingRequest()
	req.IsReplica = true
	runtime := newTestGateRuntime()
	runtime.IsReplica = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictExecute, ReasonNarrowingDuringStop)
}

func TestNarrowingWorksAtEveryTrustLevelAndMode(t *testing.T) {
	for _, trust := range []string{TrustObservation, TrustAdvisory, TrustAutonomous, "",
		"mystery"} {
		for _, mode := range []string{ExecutionManual, ExecutionAuto, ""} {
			runtime := newTestGateRuntime()
			runtime.TrustLevel, runtime.ExecutionMode = trust, mode
			runtime.Tier3Safe, runtime.Tier3Moderate = false, false
			gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
			got := gate.Authorize(context.Background(), narrowingRequest())
			if got.Verdict != VerdictExecute || got.Reason != ReasonNarrowing {
				t.Fatalf("trust %q mode %q: decision = %#v, want execute/narrowing",
					trust, mode, got)
			}
		}
	}
}

func TestNarrowingIgnoresDocumentBudgetsAndWindows(t *testing.T) {
	doc := UnattendedProfile()
	doc.AllowedChangeClasses = []ChangeClass{ChangeAnalyze} // agent_access not listed
	doc.ApprovalRequiredClasses = []ChangeClass{ChangeAgentAccess}
	doc.MaintenanceWindows = nil
	gate := newTestGate(t, gateFixture{policy: doc,
		usage: LimitUsage{SelfInitiatedChangesInWindow: 1_000_000, TablesInWindow: 1_000_000}})
	assertDecision(t, gate.Authorize(context.Background(), narrowingRequest()),
		VerdictExecute, ReasonNarrowing)

	gate = newTestGate(t, gateFixture{policyErr: errors.New("policy table gone")})
	assertDecision(t, gate.Authorize(context.Background(), narrowingRequest()),
		VerdictExecute, ReasonNarrowing)
}

func TestNarrowingOperatorApprovedStillNarrowing(t *testing.T) {
	req := narrowingRequest()
	req.OperatorApproved = true
	runtime := newTestGateRuntime()
	runtime.TrustLevel = TrustObservation
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictExecute, ReasonNarrowing)
}

func TestNarrowingStillValidatesSQL(t *testing.T) {
	req := narrowingRequest()
	req.SQL = "DROP TABLE public.orders" // caller-shaped SQL is validated (A2)
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	got := gate.Authorize(context.Background(), req)
	if got.Verdict == VerdictExecute || got.Reason != ReasonNoTypedContract {
		t.Fatalf("decision = %#v, want parked no_typed_contract", got)
	}
}

func TestNarrowingStillNeedsTypedContract(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	req := narrowingRequest()
	req.Contract.ActionType = ""
	got := gate.Authorize(context.Background(), req)
	if got.Verdict != VerdictPark || got.Reason != ReasonNoTypedContract {
		t.Fatalf("decision = %#v, want park/no_typed_contract", got)
	}
	req.Contract = &ActionContract{ActionType: "guard_freeze", RiskTier: RiskSafe,
		Narrowing: true, Guardrails: []Guardrail{"mystery"}}
	req.InternalControl = true
	got = gate.Authorize(context.Background(), req)
	if got.Verdict != VerdictBlocked || got.Reason != ReasonUnknownGuardrail {
		t.Fatalf("decision = %#v, want blocked/unknown_guardrail", got)
	}
}

func TestNarrowingStillChecksProvider(t *testing.T) {
	runtime := providerRuntime("rds")
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	req := narrowingRequest()
	req.Contract.ProviderSupport = []string{"postgres"}
	got := gate.Authorize(context.Background(), req)
	if got.Verdict != VerdictBlocked || got.Reason != ReasonProviderUnsupported {
		t.Fatalf("decision = %#v, want blocked/provider_unsupported", got)
	}
}

func TestNarrowingRuntimeUnavailableBlocks(t *testing.T) {
	gate := NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return RuntimeState{}, errors.New("control database down")
		},
	})
	got := gate.Authorize(context.Background(), narrowingRequest())
	if got.Verdict != VerdictBlocked || got.Reason != ReasonPolicyUnavailable {
		t.Fatalf("decision = %#v, want blocked/policy_unavailable", got)
	}
}

func TestNarrowingFlagAbsentKeepsStops(t *testing.T) {
	req := narrowingRequest()
	req.Contract.Narrowing = false
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	got := gate.Authorize(context.Background(), req)
	if got.Verdict != VerdictBlocked || got.Reason != ReasonEmergencyStop {
		t.Fatalf("decision = %#v, want blocked/emergency_stop", got)
	}
}

func TestNarrowingNilContractIsNotNarrowing(t *testing.T) {
	if IsNarrowing(ActionRequest{}) {
		t.Fatal("IsNarrowing(empty request) = true")
	}
	if !IsNarrowing(narrowingRequest()) {
		t.Fatal("IsNarrowing(guard_freeze) = false")
	}
}

func TestNarrowingExplainMatchesAuthorize(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	var calls []string
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, calls: &calls})
	got := mustExplainer(t, gate).Explain(context.Background(), narrowingRequest())
	if got.Verdict != VerdictExecute || got.Reason != ReasonNarrowingDuringStop {
		t.Fatalf("Explain = %#v, want execute/narrowing_during_stop", got)
	}
	assertNotCalled(t, calls, "evidence")
}

// Concurrent narrowing requests share no budget slot: none waits on or
// serializes behind another.
func TestNarrowingConcurrentRequests(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	var wg sync.WaitGroup
	results := make([]Decision, 32)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = gate.Authorize(context.Background(), narrowingRequest())
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		if got.Verdict != VerdictExecute || got.Reason != ReasonNarrowingDuringStop {
			t.Fatalf("request %d: decision = %#v", i, got)
		}
	}
}

// guard_unfreeze is a typed internal action but widening (§6.11): it is
// not narrowing, so the emergency stop, the trust level and the approval
// requirement of agent_access all still bind it.
func unfreezeRequest() ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "guard_unfreeze", RiskTier: RiskModerate,
			RollbackClass: RollbackReversible},
		InternalControl: true,
		TargetObjs:      []string{"principal:agp_aaaaaaaaaaaaaaaaaaaa"},
		Feature:         string(ChangeAgentAccess),
	}
}

func TestUnfreezeIsNotNarrowing(t *testing.T) {
	if IsNarrowing(unfreezeRequest()) {
		t.Fatal("guard_unfreeze must not be narrowing")
	}
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), unfreezeRequest()),
		VerdictBlocked, ReasonEmergencyStop)

	runtime = newTestGateRuntime()
	runtime.TrustLevel = TrustObservation
	gate = newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	approved := unfreezeRequest()
	approved.OperatorApproved = true
	assertDecision(t, gate.Authorize(context.Background(), approved),
		VerdictObserveOnly, ReasonObserveOnly)

	// Unapproved, at autonomous trust, the default document still sends it
	// to a human: agent_access is approval-required.
	gate = newTestGate(t, gateFixture{policy: StaffedProfile()})
	assertDecision(t, gate.Authorize(context.Background(), unfreezeRequest()),
		VerdictQueueApproval, ReasonApprovalRequired)
}
