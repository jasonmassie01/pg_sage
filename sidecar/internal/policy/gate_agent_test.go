package policy

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Agent-originated requests (spec §6.2.2, §6.2.3): A1-A3 as for
// any request, then A4 (GateConfig.Agents), A5 (the level: decider cap,
// operator ceiling, rollback-class cap) and A6 (the change-class
// allowlist), mapped to verdicts by level.

type fakeDecider struct {
	mu       sync.Mutex
	decision Decision
	level    int
	stop     bool
	seen     []ActionRequest
}

func (f *fakeDecider) Decide(_ context.Context, req ActionRequest) (Decision, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, req)
	return f.decision, f.level, f.stop
}

func (f *fakeDecider) calls() []ActionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ActionRequest(nil), f.seen...)
}

// newAgentGate is newTestGate with GateConfig.Agents set; recorded holds
// every decision the gate recorded with its request.
func newAgentGate(t *testing.T, fixture gateFixture, agents AgentDecider,
	recorded *[]ActionRequest) Gate {
	t.Helper()
	inner := newTestGate(t, fixture).(*authorizationGate)
	cfg := inner.config
	cfg.Agents = agents
	record := cfg.RecordDecision
	var mu sync.Mutex
	cfg.RecordDecision = func(ctx context.Context, req ActionRequest, d Decision) (
		string, error) {
		if recorded != nil {
			mu.Lock()
			*recorded = append(*recorded, req)
			mu.Unlock()
		}
		return record(ctx, req, d)
	}
	return NewGate(cfg)
}

func agentRef() *PrincipalRef {
	return &PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", SponsorID: 7,
		TaskID: "task-1", Tool: "optimize_query"}
}

// reversibleAgentRequest is an index build an agent asked for: safe and
// reversible, so the rollback-class cap does not hold it.
func reversibleAgentRequest() ActionRequest {
	req := validIndexRequest()
	req.Contract.RollbackClass = RollbackReversible
	req.Principal = agentRef()
	return req
}

func runtimeAt(trust string) RuntimeState {
	r := newTestGateRuntime()
	r.TrustLevel = trust
	return r
}

func TestAgentRequestWithoutDeciderIsCappedAtApproval(t *testing.T) {
	// Governance off: the request would execute for pg_sage (autonomous,
	// tier3, ramp satisfied) but an agent's is capped at L2.
	gate := newAgentGate(t, gateFixture{}, nil, nil)
	got := gate.Authorize(context.Background(), reversibleAgentRequest())
	assertDecision(t, got, VerdictQueueApproval, ReasonApprovalRequired)
	// The same request without a principal still executes: no regression.
	req := reversibleAgentRequest()
	req.Principal = nil
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictExecute, ReasonAuthorized)
}

func TestAgentLevelMapsToVerdict(t *testing.T) {
	cases := []struct {
		level   int
		verdict Verdict
		reason  Reason
	}{
		{0, VerdictBlocked, ReasonAgentLevel0},
		{1, VerdictObserveOnly, ReasonAgentProposalRecorded},
		{2, VerdictQueueApproval, ReasonApprovalRequired},
		// G1 has no L3 envelope: L3 falls back to L2 (§6.2.3).
		{3, VerdictQueueApproval, ReasonApprovalRequired},
		{-1, VerdictBlocked, ReasonAgentLevel0},
		{9, VerdictQueueApproval, ReasonApprovalRequired},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("L%d", tc.level), func(t *testing.T) {
			agents := &fakeDecider{level: tc.level}
			gate := newAgentGate(t, gateFixture{}, agents, nil)
			got := gate.Authorize(context.Background(), reversibleAgentRequest())
			assertDecision(t, got, tc.verdict, tc.reason)
			if len(agents.calls()) != 1 {
				t.Fatalf("decider calls = %d, want 1", len(agents.calls()))
			}
		})
	}
}

func TestAgentDStepFailureWinsAndIsFinal(t *testing.T) {
	for _, reason := range []Reason{"agent_frozen", "agent_unsponsored",
		"agent_env_ceiling", "agent_lease_expired"} {
		t.Run(string(reason), func(t *testing.T) {
			var calls []string
			agents := &fakeDecider{stop: true, decision: Decision{
				Verdict: VerdictBlocked, Reason: reason, Detail: "why"}}
			gate := newAgentGate(t, gateFixture{calls: &calls}, agents, nil)
			got := gate.Authorize(context.Background(), reversibleAgentRequest())
			assertDecision(t, got, VerdictBlocked, reason)
			if got.Detail != "why" || got.RiskTier != RiskSafe {
				t.Fatalf("decision = %#v, want the decider's detail and the tier", got)
			}
			assertNotCalled(t, calls, "policy", "usage", "window")
		})
	}
}

func TestAgentRateParks(t *testing.T) {
	agents := &fakeDecider{stop: true, decision: Decision{Verdict: VerdictPark,
		Reason: "agent_rate", Detail: "retry_after=60s"}}
	gate := newAgentGate(t, gateFixture{}, agents, nil)
	got := gate.Authorize(context.Background(), reversibleAgentRequest())
	assertDecision(t, got, VerdictPark, "agent_rate")
}

func TestAgentDeciderVerdictOtherThanBlockOrParkFailsClosed(t *testing.T) {
	// A decider that "stops" with execute must not authorize anything.
	agents := &fakeDecider{stop: true, decision: Decision{Verdict: VerdictExecute,
		Reason: ReasonAuthorized}}
	gate := newAgentGate(t, gateFixture{}, agents, nil)
	got := gate.Authorize(context.Background(), reversibleAgentRequest())
	if got.Verdict != VerdictBlocked {
		t.Fatalf("verdict = %q, want blocked", got.Verdict)
	}
}

func TestAgentOperatorCeilingByTrustLevel(t *testing.T) {
	cases := []struct {
		trust   string
		verdict Verdict
		reason  Reason
	}{
		{TrustObservation, VerdictObserveOnly, ReasonAgentProposalRecorded},
		{TrustAdvisory, VerdictQueueApproval, ReasonApprovalRequired},
		{TrustAutonomous, VerdictQueueApproval, ReasonApprovalRequired},
		{"", VerdictBlocked, ReasonUnknownTrustLevel},
		{"mystery", VerdictBlocked, ReasonUnknownTrustLevel},
	}
	for _, tc := range cases {
		t.Run(tc.trust, func(t *testing.T) {
			agents := &fakeDecider{level: 3}
			gate := newAgentGate(t, gateFixture{runtime: runtimeAt(tc.trust),
				runtimeSet: true}, agents, nil)
			got := gate.Authorize(context.Background(), reversibleAgentRequest())
			assertDecision(t, got, tc.verdict, tc.reason)
		})
	}
}

func TestAgentManualExecutionModeRecordsProposal(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.ExecutionMode = ExecutionManual
	gate := newAgentGate(t, gateFixture{runtime: runtime, runtimeSet: true},
		&fakeDecider{level: 3}, nil)
	assertDecision(t, gate.Authorize(context.Background(), reversibleAgentRequest()),
		VerdictObserveOnly, ReasonAgentProposalRecorded)
}

func TestAgentRollbackClassCap(t *testing.T) {
	for _, class := range []RollbackClass{RollbackApplication, RollbackMitigationOnly,
		RollbackForwardFixOnly, RollbackNotReversible, ""} {
		if got := agentRollbackCap(class); got != 2 {
			t.Fatalf("agentRollbackCap(%q) = %d, want 2", class, got)
		}
	}
	for _, class := range []RollbackClass{RollbackReversible, RollbackNoRollbackNeeded,
		RollbackNotApplicable} {
		if got := agentRollbackCap(class); got != 3 {
			t.Fatalf("agentRollbackCap(%q) = %d, want 3", class, got)
		}
	}
}

func TestAgentOperatorCeilingValues(t *testing.T) {
	want := map[string]int{TrustObservation: 1, TrustAdvisory: 2, TrustAutonomous: 3,
		"": 0, "other": 0}
	for trust, level := range want {
		if got := agentOperatorCeiling(trust); got != level {
			t.Fatalf("agentOperatorCeiling(%q) = %d, want %d", trust, got, level)
		}
	}
}

func TestAgentChangeClassNotAllowedBlocksAtL2(t *testing.T) {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.AllowedChangeClasses = []ChangeClass{ChangeAnalyze}
	gate := newAgentGate(t, gateFixture{policy: doc}, &fakeDecider{level: 2}, nil)
	assertDecision(t, gate.Authorize(context.Background(), reversibleAgentRequest()),
		VerdictBlocked, ReasonChangeClassNotAllowed)
}

func TestAgentPolicyUnavailableBlocksAtL2(t *testing.T) {
	gate := newAgentGate(t, gateFixture{policyErr: ErrPolicyNotFound},
		&fakeDecider{level: 2}, nil)
	assertDecision(t, gate.Authorize(context.Background(), reversibleAgentRequest()),
		VerdictBlocked, ReasonPolicyUnavailable)
}

func TestAgentOperatorApprovedRunsOperatorDecision(t *testing.T) {
	req := reversibleAgentRequest()
	req.OperatorApproved = true
	gate := newAgentGate(t, gateFixture{}, &fakeDecider{level: 2}, nil)
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictExecute, ReasonOperatorApproved)
	// Observation still means observe_only for an approved agent request.
	gate = newAgentGate(t, gateFixture{runtime: runtimeAt(TrustObservation),
		runtimeSet: true}, &fakeDecider{level: 2}, nil)
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictObserveOnly, ReasonObserveOnly)
}

func TestAgentOperatorApprovedStillBoundByDSteps(t *testing.T) {
	req := reversibleAgentRequest()
	req.OperatorApproved = true
	agents := &fakeDecider{stop: true, decision: Decision{Verdict: VerdictBlocked,
		Reason: "agent_frozen"}}
	gate := newAgentGate(t, gateFixture{}, agents, nil)
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, "agent_frozen")
	if !agents.calls()[0].OperatorApproved {
		t.Fatal("the decider must see OperatorApproved (D8 and D9 do not bind it)")
	}
}

func TestAgentOperatorApprovedBelowL2IsNotExecuted(t *testing.T) {
	req := reversibleAgentRequest()
	req.OperatorApproved = true
	gate := newAgentGate(t, gateFixture{}, &fakeDecider{level: 0}, nil)
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, ReasonAgentLevel0)
}

func TestAgentHardStopAndValidationPrecedeDecider(t *testing.T) {
	agents := &fakeDecider{level: 3}
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newAgentGate(t, gateFixture{runtime: runtime, runtimeSet: true}, agents, nil)
	assertDecision(t, gate.Authorize(context.Background(), reversibleAgentRequest()),
		VerdictBlocked, ReasonEmergencyStop)
	req := reversibleAgentRequest()
	req.Contract = nil
	gate = newAgentGate(t, gateFixture{}, agents, nil)
	if got := gate.Authorize(context.Background(), req); got.Reason != ReasonNoTypedContract {
		t.Fatalf("reason = %q, want no_typed_contract", got.Reason)
	}
	req = reversibleAgentRequest()
	req.SQL = "DROP TABLE sage.findings"
	if got := gate.Authorize(context.Background(), req); got.Reason != ReasonNoTypedContract {
		t.Fatalf("reason = %q, want no_typed_contract (A2 SQL validation)", got.Reason)
	}
	if n := len(agents.calls()); n != 0 {
		t.Fatalf("decider calls = %d, want 0: A1 and A2 decide first", n)
	}
}

func TestAgentPrincipalFilledFromContext(t *testing.T) {
	agents := &fakeDecider{level: 2}
	var recorded []ActionRequest
	gate := newAgentGate(t, gateFixture{}, agents, &recorded)
	req := reversibleAgentRequest()
	req.Principal = nil
	ctx := WithPrincipalRef(context.Background(), *agentRef())
	assertDecision(t, gate.Authorize(ctx, req), VerdictQueueApproval,
		ReasonApprovalRequired)
	seen := agents.calls()
	if len(seen) != 1 || seen[0].Principal == nil ||
		seen[0].Principal.ID != agentRef().ID || seen[0].Principal.TaskID != "task-1" {
		t.Fatalf("decider saw %#v, want the context principal", seen)
	}
	if len(recorded) != 1 || recorded[0].Principal == nil {
		t.Fatalf("recorded = %#v, want the principal on the recorded request", recorded)
	}
	// Explain fills it too.
	got := mustExplainer(t, gate).Explain(ctx, req)
	if got.Verdict != VerdictQueueApproval || len(agents.calls()) != 2 {
		t.Fatalf("Explain = %#v, want it decided as an agent request", got)
	}
}

func TestAgentExplicitPrincipalBeatsContext(t *testing.T) {
	agents := &fakeDecider{level: 2}
	gate := newAgentGate(t, gateFixture{}, agents, nil)
	ctx := WithPrincipalRef(context.Background(), PrincipalRef{ID: "agp_other"})
	gate.Authorize(ctx, reversibleAgentRequest())
	if got := agents.calls()[0].Principal.ID; got != agentRef().ID {
		t.Fatalf("principal = %q, want the request's own", got)
	}
}

func TestPrincipalRefFromContextEmpty(t *testing.T) {
	if _, ok := PrincipalRefFromContext(context.Background()); ok {
		t.Fatal("an empty context must carry no principal")
	}
	ref, ok := PrincipalRefFromContext(WithPrincipalRef(context.Background(),
		PrincipalRef{}))
	if !ok || ref.ID != "" {
		t.Fatalf("ref = %#v ok=%v, want the unbound agent (ID \"\")", ref, ok)
	}
}

func TestUnboundAgentIsStillAgentOriginated(t *testing.T) {
	// The stdio client without mcp.stdio_principal: ID "" is an agent.
	gate := newAgentGate(t, gateFixture{}, nil, nil)
	ctx := WithPrincipalRef(context.Background(), PrincipalRef{Tool: "optimize_query"})
	req := reversibleAgentRequest()
	req.Principal = nil
	assertDecision(t, gate.Authorize(ctx, req), VerdictQueueApproval,
		ReasonApprovalRequired)
}

func TestNarrowingSkipsAgentStepsEvenForFrozenPrincipal(t *testing.T) {
	// Containment is never blocked by the frozen agent's own context
	// (coordinator decision, 2026-10-10): narrowing skips A4.
	agents := &fakeDecider{stop: true, decision: Decision{Verdict: VerdictBlocked,
		Reason: "agent_frozen"}}
	gate := newAgentGate(t, gateFixture{runtime: runtimeAt(TrustObservation),
		runtimeSet: true}, agents, nil)
	ctx := WithPrincipalRef(context.Background(), *agentRef())
	assertDecision(t, gate.Authorize(ctx, narrowingRequest()),
		VerdictExecute, ReasonNarrowing)
	if n := len(agents.calls()); n != 0 {
		t.Fatalf("decider calls = %d, want 0 for a narrowing contract", n)
	}
}

func TestGuardUnfreezeIsWideningAndNeedsApproval(t *testing.T) {
	// guard_unfreeze restores grants: it is not Narrowing and queues for a
	// person even at autonomous trust, with or without an agent.
	req := ActionRequest{
		Contract: &ActionContract{ActionType: "guard_unfreeze", RiskTier: RiskModerate,
			RollbackClass: RollbackReversible},
		InternalControl: true, Feature: string(ChangeAgentAccess),
		TargetObjs: []string{"principal:agp_aaaaaaaaaaaaaaaaaaaa"},
	}
	if IsNarrowing(req) {
		t.Fatal("guard_unfreeze must not be narrowing")
	}
	gate := newAgentGate(t, gateFixture{}, &fakeDecider{level: 3}, nil)
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictQueueApproval, ReasonApprovalRequired)
	req.Principal = agentRef()
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictQueueApproval, ReasonApprovalRequired)
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	req.Principal = nil
	gate = newAgentGate(t, gateFixture{runtime: runtime, runtimeSet: true}, nil, nil)
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, ReasonEmergencyStop)
}

func TestAgentDecisionsConcurrent(t *testing.T) {
	agents := &fakeDecider{level: 2}
	gate := newAgentGate(t, gateFixture{}, agents, nil)
	var wg sync.WaitGroup
	const n = 32
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := WithPrincipalRef(context.Background(),
				PrincipalRef{ID: fmt.Sprintf("agp_%020d", i)})
			req := reversibleAgentRequest()
			req.Principal = nil
			if got := gate.Authorize(ctx, req); got.Verdict != VerdictQueueApproval {
				t.Errorf("verdict = %q", got.Verdict)
			}
		}(i)
	}
	wg.Wait()
	ids := map[string]bool{}
	for _, req := range agents.calls() {
		ids[req.Principal.ID] = true
	}
	if len(ids) != n {
		t.Fatalf("distinct principals seen = %d, want %d (no cross-request leak)",
			len(ids), n)
	}
}

func TestAgentGateDoesNotSpendBudgetsForQueuedRequests(t *testing.T) {
	var calls []string
	gate := newAgentGate(t, gateFixture{calls: &calls}, &fakeDecider{level: 2}, nil)
	gate.Authorize(context.Background(), reversibleAgentRequest())
	assertNotCalled(t, calls, "usage", "window")
}
