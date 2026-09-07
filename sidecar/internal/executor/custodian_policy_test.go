package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

type custodianGateCapture struct {
	request policy.ActionRequest
	verdict policy.Decision
	calls   int
}

func (g *custodianGateCapture) Authorize(
	_ context.Context, request policy.ActionRequest,
) policy.Decision {
	g.calls++
	g.request = request
	return g.verdict
}

func TestCustodianProposalUsesExecutorStandingPolicyGate(t *testing.T) {
	gate := &custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
	}}
	exec := New(nil, &config.Config{}, nil, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(gate)
	proposal := CustodianProposal{
		Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."orders"`,
		TargetObjects: []string{"public.orders"},
		Evidence:      map[string]any{"xmin_age": int64(1234)},
		Deadline: &policy.DeadlineContext{
			Kind: policy.DeadlineXID, Urgency: policy.UrgencyCritical,
			HardAt: time.Now().Add(time.Hour),
		},
	}
	decision := exec.EvaluateCustodianProposal(context.Background(), proposal)
	if decision.Decision != PolicyDecisionExecute {
		t.Fatalf("decision = %#v, want execute", decision)
	}
	if gate.request.Feature != proposal.Feature || gate.request.SQL != proposal.SQL {
		t.Fatalf("standing gate request = %#v", gate.request)
	}
	if len(gate.request.TargetObjs) != 1 || gate.request.TargetObjs[0] != "public.orders" {
		t.Fatalf("standing gate targets = %#v", gate.request.TargetObjs)
	}
	if gate.request.Deadline != proposal.Deadline {
		t.Fatalf("standing gate deadline = %#v, want %#v", gate.request.Deadline,
			proposal.Deadline)
	}
	if gate.request.Evidence["xmin_age"] != int64(1234) {
		t.Fatalf("standing gate evidence = %#v", gate.request.Evidence)
	}
}

func TestMaxSlotWALKeepSizeBackstopIsTypedAndAuthorized(t *testing.T) {
	const sql = "ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'"
	if err := ValidateExecutorSQL(sql); err != nil {
		t.Fatalf("max_slot_wal_keep_size backstop rejected: %v", err)
	}
	gate := &custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
	}}
	exec := New(nil, &config.Config{}, nil, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(gate)
	decision := exec.EvaluateCustodianProposal(context.Background(), CustodianProposal{
		Feature: "wal", SQL: sql, TargetObjects: []string{"slot:orders_cdc"},
	})
	if decision.Decision != PolicyDecisionExecute {
		t.Fatalf("backstop decision = %#v, want execute", decision)
	}
	if gate.request.Contract == nil ||
		gate.request.Contract.ActionType != "alter_system_guc" {
		t.Fatalf("backstop contract = %#v, want typed GUC action", gate.request.Contract)
	}
	if gate.request.Feature != string(policy.ChangeConfigGUC) {
		t.Fatalf("backstop feature = %q, want config_guc", gate.request.Feature)
	}
}

func zeroTime() (result time.Time) { return result }
