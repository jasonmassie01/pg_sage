package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Operator approvals go through the standing gate (decision 2026-09-26):
// the gate records the operator decision and can refuse it.

type operatorGateSpy struct {
	requests []policy.ActionRequest
	doc      policy.Document
	now      time.Time
}

func (s *operatorGateSpy) gate() policy.Gate {
	return policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true, TrustLevel: policy.TrustAdvisory,
				ExecutionMode: policy.ExecutionApproval, InConfiguredWindow: true}, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return s.doc, nil
		},
		RecordDecisionDetailed: func(_ context.Context, req policy.ActionRequest,
			_ policy.Decision) (string, int64, error) {
			s.requests = append(s.requests, req)
			return "ev", int64(len(s.requests)), nil
		},
		Now: func() time.Time { return s.now },
	})
}

func TestExecuteManualFailsClosedWithoutGate(t *testing.T) {
	exec := New(nil, autonomousTestConfig(), nil, time.Time{}, noopExecLog)
	_, err := exec.ExecuteManual(context.Background(), 1, "ANALYZE public.orders", "", nil)
	if err == nil || !strings.Contains(err.Error(), reasonNoStandingPolicy) {
		t.Fatalf("ExecuteManual without gate: err = %v, want %q", err, reasonNoStandingPolicy)
	}
}

func TestOperatorAuthorizationIsRecordedAsOperatorIntent(t *testing.T) {
	spy := &operatorGateSpy{doc: policy.UnattendedProfile(), now: time.Now()}
	exec := New(nil, autonomousTestConfig(), nil, time.Time{}, noopExecLog)
	exec.WithPolicyGate(spy.gate())
	approver := 7

	decisionID, err := exec.authorizeOperatorAction(
		context.Background(), "ANALYZE public.orders", 42, &approver)

	if err != nil || decisionID != 1 {
		t.Fatalf("authorize: id=%d err=%v", decisionID, err)
	}
	req := spy.requests[0]
	if !req.OperatorApproved || req.Feature != string(policy.ChangeAnalyze) {
		t.Fatalf("gate request = %+v, want operator-approved analyze", req)
	}
	if req.Evidence["approved_by"] != 7 || req.Evidence["finding_id"] != 42 {
		t.Fatalf("evidence = %v", req.Evidence)
	}
	input := ledgerInput(nil, 3, req, policy.Decision{Verdict: policy.VerdictExecute})
	if input.Intent != operatorDecisionIntent {
		t.Fatalf("ledger intent = %q, want %q (excluded from self-initiated usage)",
			input.Intent, operatorDecisionIntent)
	}
}

func TestOperatorAuthorizationRefusedByPolicyWindow(t *testing.T) {
	// Sunday 02:00 UTC is outside the staffed profile's weekday window.
	sunday := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	spy := &operatorGateSpy{doc: policy.StaffedProfile(), now: sunday}
	exec := New(nil, autonomousTestConfig(), nil, time.Time{}, noopExecLog)
	exec.WithPolicyGate(spy.gate())

	_, err := exec.authorizeOperatorAction(context.Background(),
		"DROP INDEX CONCURRENTLY public.idx_orders_old", 42, nil)
	if err == nil || !strings.Contains(err.Error(), "outside maintenance window") {
		t.Fatalf("err = %v, want outside maintenance window refusal", err)
	}
}

func TestApprovalReadinessUsesGateOperatorPath(t *testing.T) {
	sunday := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	action := store.QueuedAction{ID: 1, Status: "pending",
		ActionType: "drop_unused_index", ActionRisk: "moderate",
		ProposedSQL: "DROP INDEX CONCURRENTLY public.idx_orders_old"}
	cfg := &config.Config{Trust: config.TrustConfig{Level: "advisory"}}

	closed := New(nil, cfg, nil, time.Time{}, noopExecLog)
	closed.WithPolicyGate((&operatorGateSpy{doc: policy.StaffedProfile(), now: sunday}).gate())
	got := closed.ApprovalReadiness(action, sunday)
	if got.Eligible || got.DeferReason != "outside maintenance window" {
		t.Fatalf("closed window readiness = %+v, want deferred", got)
	}

	open := New(nil, cfg, nil, time.Time{}, noopExecLog)
	spy := &operatorGateSpy{doc: policy.UnattendedProfile(), now: sunday}
	open.WithPolicyGate(spy.gate())
	got = open.ApprovalReadiness(action, sunday)
	if !got.Eligible || got.Policy.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("open window readiness = %+v, want eligible ready approval", got)
	}
	if len(spy.requests) != 0 {
		t.Fatalf("readiness recorded %d ledger decisions, want 0", len(spy.requests))
	}
}

// Operator SQL without a typed contract fails closed; every recommendation
// the product emits maps to one.
func TestOperatorActionWithoutTypedContractIsRefused(t *testing.T) {
	spy := &operatorGateSpy{doc: policy.UnattendedProfile(), now: time.Now()}
	exec := New(nil, autonomousTestConfig(), nil, time.Time{}, noopExecLog)
	exec.WithPolicyGate(spy.gate())
	_, err := exec.authorizeOperatorAction(context.Background(),
		"CREATE INDEX idx_blocking ON public.orders (id)", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "no typed contract") {
		t.Fatalf("untyped operator SQL: err = %v, want no typed contract refusal", err)
	}
}
