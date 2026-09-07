package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGateDecisionOrderingHardStopsBeforeContractAndPolicy(t *testing.T) {
	tests := []struct {
		name    string
		runtime RuntimeState
		want    Reason
	}{
		{"executor disabled", RuntimeState{ExecutorEnabled: false}, ReasonExecutorDisabled},
		{
			"emergency stop",
			RuntimeState{ExecutorEnabled: true, EmergencyStop: true},
			ReasonEmergencyStop,
		},
		{
			"replica mutation",
			RuntimeState{ExecutorEnabled: true, IsReplica: true},
			ReasonReplicaMutation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := []string{}
			gate := newTestGate(t, gateFixture{
				runtime:    tt.runtime,
				runtimeSet: true,
				calls:      &calls,
				policyErr: errors.New(
					"policy must not be reached before hard stop"),
			})
			req := validIndexRequest()
			if tt.want == ReasonReplicaMutation {
				req.Contract = &ActionContract{
					ActionType: "create_index_concurrently",
					RiskTier:   RiskSafe,
				}
			}

			decision := gate.Authorize(context.Background(), req)

			assertDecision(t, decision, VerdictBlocked, tt.want)
			assertCallPrefix(t, calls, []string{"runtime", "evidence"})
			assertNotCalled(t, calls, "validate_sql", "policy", "usage")
		})
	}
}

func TestGateParksMissingOrInvalidTypedContractBeforePolicy(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ActionRequest)
	}{
		{"missing contract", func(req *ActionRequest) { req.Contract = nil }},
		{"invalid sql", func(req *ActionRequest) { req.SQL = "DROP SCHEMA sage CASCADE" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := []string{}
			gate := newTestGate(t, gateFixture{calls: &calls})
			req := validIndexRequest()
			tt.edit(&req)

			decision := gate.Authorize(context.Background(), req)

			assertDecision(t, decision, VerdictPark, ReasonNoTypedContract)
			assertNotCalled(t, calls, "policy", "usage")
		})
	}
}

func TestGateObservationAndManualPrecedePolicyLookup(t *testing.T) {
	tests := []RuntimeState{
		{
			ExecutorEnabled: true,
			TrustLevel:      TrustObservation,
			ExecutionMode:   ExecutionAuto,
		},
		{
			ExecutorEnabled: true,
			TrustLevel:      TrustAutonomous,
			ExecutionMode:   ExecutionManual,
		},
	}
	for _, runtime := range tests {
		calls := []string{}
		gate := newTestGate(t, gateFixture{
			runtime: runtime, runtimeSet: true, calls: &calls,
		})

		decision := gate.Authorize(context.Background(), validIndexRequest())

		assertDecision(t, decision, VerdictObserveOnly, ReasonObserveOnly)
		assertNotCalled(t, calls, "policy", "usage")
	}
}

func TestGateFailsClosedWhenPolicyMissingOrInvalid(t *testing.T) {
	for _, policyErr := range []error{ErrPolicyNotFound, ErrPolicyInvalid} {
		t.Run(policyErr.Error(), func(t *testing.T) {
			calls := []string{}
			gate := newTestGate(t, gateFixture{calls: &calls, policyErr: policyErr})

			decision := gate.Authorize(context.Background(), validIndexRequest())

			assertDecision(t, decision, VerdictBlocked, ReasonPolicyUnavailable)
			assertCallPrefix(t, calls,
				[]string{"runtime", "validate_sql", "policy", "evidence"})
			assertNotCalled(t, calls, "usage")
		})
	}
}

func TestApprovalGuardrailPrecedesBudgetsAndWindows(t *testing.T) {
	calls := []string{}
	gate := newTestGate(t, gateFixture{
		calls: &calls,
		usage: LimitUsage{StorageBytes: 999999, TablesInWindow: 999},
	})
	req := validIndexRequest()
	req.Contract.Guardrails = []Guardrail{GuardrailApprovalRequired}

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictQueueApproval, ReasonApprovalRequired)
	if len(decision.Guardrails) != 1 ||
		decision.Guardrails[0] != GuardrailApprovalRequired {
		t.Fatalf("Guardrails = %#v", decision.Guardrails)
	}
	assertNotCalled(t, calls, "usage")
}

func TestStandingPolicyChangeClassesConstrainAuthority(t *testing.T) {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.AllowedChangeClasses = []ChangeClass{ChangeAnalyze}
	gate := newTestGate(t, gateFixture{policy: doc})

	decision := gate.Authorize(context.Background(), validIndexRequest())

	assertDecision(t, decision, VerdictBlocked, ReasonChangeClassNotAllowed)
}

func TestStandingPolicyApprovalClassQueuesDespiteSafeTier(t *testing.T) {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.ApprovalRequiredClasses = []ChangeClass{ChangeIndex}
	gate := newTestGate(t, gateFixture{policy: doc})

	decision := gate.Authorize(context.Background(), validIndexRequest())

	assertDecision(t, decision, VerdictQueueApproval, ReasonApprovalRequired)
}

func TestUnknownGuardrailFailsClosed(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	req := validIndexRequest()
	req.Contract.Guardrails = []Guardrail{Guardrail("unknown_safety_rule")}

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictBlocked, ReasonUnknownGuardrail)
	if !strings.Contains(decision.Detail, "unknown_safety_rule") {
		t.Fatalf("Detail = %q, want unknown guardrail name", decision.Detail)
	}
}

func TestZeroBudgetParksWhileNullBudgetHasNoCap(t *testing.T) {
	tests := []struct {
		name   string
		budget BudgetLimit
		want   Verdict
		reason Reason
	}{
		{"zero means none allowed", NewBudgetLimit(0), VerdictPark, ReasonBudgetExceeded},
		{"null means no cap", NoCapBudget(), VerdictExecute, ReasonAuthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := StaffedProfile()
			doc.MaintenanceWindows = []string{"always"}
			doc.Budgets.StorageBytes = tt.budget
			gate := newTestGate(t, gateFixture{
				policy: doc,
				usage:  LimitUsage{StorageBytes: 1},
			})

			decision := gate.Authorize(context.Background(), validIndexRequest())

			assertDecision(t, decision, tt.want, tt.reason)
		})
	}
}

func TestBudgetPrecedesMaintenanceWindow(t *testing.T) {
	calls := []string{}
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"0 2 * * 0"}
	doc.Budgets.StorageBytes = NewBudgetLimit(0)
	gate := newTestGate(t, gateFixture{
		calls:  &calls,
		policy: doc,
		usage:  LimitUsage{StorageBytes: 1},
		now:    time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	})

	decision := gate.Authorize(context.Background(), validIndexRequest())

	assertDecision(t, decision, VerdictPark, ReasonBudgetExceeded)
	assertNotCalled(t, calls, "window")
}

func TestBudgetBlastRadiusAndRateViolationsPark(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*Document)
		usage LimitUsage
		want  Reason
	}{
		{
			name: "storage budget",
			edit: func(doc *Document) {
				doc.Budgets.StorageBytes = NewBudgetLimit(10)
			},
			usage: LimitUsage{StorageBytes: 11},
			want:  ReasonBudgetExceeded,
		},
		{
			name: "rows rewritten blast radius",
			edit: func(doc *Document) {
				doc.BlastRadius.MaxRowsRewritten = 100
			},
			usage: LimitUsage{RowsRewritten: 101},
			want:  ReasonBlastRadiusExceeded,
		},
		{
			name: "tables per window blast radius",
			edit: func(doc *Document) {
				doc.BlastRadius.MaxTablesPerWindow = 2
			},
			usage: LimitUsage{TablesInWindow: 3},
			want:  ReasonBlastRadiusExceeded,
		},
		{
			name: "self initiated change rate",
			edit: func(doc *Document) {
				doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 4
			},
			usage: LimitUsage{SelfInitiatedChangesInWindow: 4},
			want:  ReasonRateLimitExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := UnattendedProfile()
			doc.MaintenanceWindows = []string{"always"}
			tt.edit(&doc)
			gate := newTestGate(t, gateFixture{policy: doc, usage: tt.usage})

			decision := gate.Authorize(context.Background(), validIndexRequest())

			assertDecision(t, decision, VerdictPark, tt.want)
		})
	}
}

func TestModerateActionOutsideWindowBlocksWithoutDeadline(t *testing.T) {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"0 2 * * 0"}
	gate := newTestGate(t, gateFixture{
		policy: doc,
		now:    time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	})
	req := validIndexRequest()
	req.Contract.RiskTier = RiskModerate

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictBlocked, ReasonOutsideMaintenanceWindow)
	if decision.OffWindowOK {
		t.Fatal("OffWindowOK = true without deadline")
	}
}

func TestDeadlineOverrideExecutesOutsideWindowAndIsEvidenced(t *testing.T) {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"0 2 * * 0"}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gate := newTestGate(t, gateFixture{policy: doc, now: now})
	req := validIndexRequest()
	req.Feature = "freeze"
	req.Contract.RiskTier = RiskModerate
	req.Deadline = &DeadlineContext{
		Kind: DeadlineXID, Urgency: UrgencyCritical, HardAt: now.Add(90 * time.Minute),
	}

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictExecute, ReasonDeadlineOverride)
	if !decision.OffWindowOK {
		t.Fatal("OffWindowOK = false, want true")
	}
	if decision.EvidenceID == "" {
		t.Fatal("EvidenceID empty for deadline override")
	}
}

func TestDeadlineCannotOverrideWhenProfileDisallowsKind(t *testing.T) {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"0 2 * * 0"}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gate := newTestGate(t, gateFixture{policy: doc, now: now})
	req := validIndexRequest()
	req.Contract.RiskTier = RiskModerate
	req.Deadline = &DeadlineContext{
		Kind: DeadlineDisk, Urgency: UrgencyCritical, HardAt: now.Add(time.Hour),
	}

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictBlocked, ReasonOutsideMaintenanceWindow)
	if decision.OffWindowOK {
		t.Fatal("OffWindowOK = true for disallowed deadline override")
	}
}

func TestExpiredOrUnknownDeadlineCannotOverrideWindow(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	for _, deadline := range []*DeadlineContext{
		{Kind: DeadlineXID, Urgency: UrgencyCritical, HardAt: now.Add(-time.Minute)},
		{Kind: DeadlineKind("unknown"), Urgency: UrgencyCritical, HardAt: now.Add(time.Hour)},
	} {
		doc := UnattendedProfile()
		doc.MaintenanceWindows = []string{"0 2 * * 0"}
		gate := newTestGate(t, gateFixture{policy: doc, now: now})
		req := validIndexRequest()
		req.Contract.RiskTier = RiskModerate
		req.Deadline = deadline

		decision := gate.Authorize(context.Background(), req)

		assertDecision(t, decision, VerdictBlocked, ReasonOutsideMaintenanceWindow)
		if decision.OffWindowOK {
			t.Fatalf("OffWindowOK = true for deadline %#v", deadline)
		}
	}
}

func TestUnknownRiskTierFailsClosed(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	req := validIndexRequest()
	req.Contract.RiskTier = RiskTier("mystery")

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictBlocked, ReasonUnknownRiskTier)
}

type gateFixture struct {
	runtime    RuntimeState
	runtimeSet bool
	policy     Document
	policyErr  error
	usage      LimitUsage
	now        time.Time
	calls      *[]string
}

func newTestGate(t *testing.T, fixture gateFixture) Gate {
	t.Helper()
	if !fixture.runtimeSet {
		fixture.runtime = RuntimeState{
			ExecutorEnabled: true,
			TrustLevel:      TrustAutonomous,
			ExecutionMode:   ExecutionAuto,
		}
	}
	if fixture.policy.Profile == "" {
		fixture.policy = UnattendedProfile()
		fixture.policy.MaintenanceWindows = []string{"always"}
	}
	if fixture.now.IsZero() {
		fixture.now = time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	}
	appendCall := func(name string) {
		if fixture.calls != nil {
			*fixture.calls = append(*fixture.calls, name)
		}
	}
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			appendCall("runtime")
			return fixture.runtime, nil
		},
		ValidateSQL: func(sql string) error {
			appendCall("validate_sql")
			if strings.Contains(strings.ToLower(sql), "sage") ||
				!strings.Contains(strings.ToUpper(sql), "CONCURRENTLY") {
				return fmt.Errorf("unsafe SQL")
			}
			return nil
		},
		Policy: func(context.Context, ActionRequest) (Document, error) {
			appendCall("policy")
			return fixture.policy, fixture.policyErr
		},
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			appendCall("usage")
			return fixture.usage, nil
		},
		WindowObserved: func() { appendCall("window") },
		RecordDecision: func(
			_ context.Context, _ ActionRequest, decision Decision,
		) (string, error) {
			appendCall("evidence")
			return "decision-1", nil
		},
		Now: func() time.Time { return fixture.now },
	})
}

func validIndexRequest() ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{
			ActionType: "create_index_concurrently",
			RiskTier:   RiskSafe,
		},
		SQL:        `CREATE INDEX CONCURRENTLY idx_orders ON public.orders (status)`,
		TargetObjs: []string{"public.orders"},
		Feature:    "index",
	}
}

func assertDecision(t *testing.T, got Decision, verdict Verdict, reason Reason) {
	t.Helper()
	if got.Verdict != verdict || got.Reason != reason {
		t.Fatalf("Decision = %#v, want verdict=%q reason=%q", got, verdict, reason)
	}
	if got.EvidenceID == "" {
		t.Fatal("Decision.EvidenceID is empty")
	}
}

func assertCallPrefix(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("calls = %#v, want prefix %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %#v, want prefix %#v", got, want)
		}
	}
}

func assertNotCalled(t *testing.T, calls []string, names ...string) {
	t.Helper()
	for _, call := range calls {
		for _, name := range names {
			if call == name {
				t.Fatalf("calls = %#v, %q must not be called", calls, name)
			}
		}
	}
}
