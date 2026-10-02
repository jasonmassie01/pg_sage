package executor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// A pre-incident investigation shows the standing gate's verdict for the
// custodian action that addresses it without recording a decision: the
// gate's Explain runs, Authorize never does, and without an explaining
// gate the verdict fails closed.

type explainingGate struct {
	custodianGateCapture
	explained int
}

func (g *explainingGate) Explain(_ context.Context, r policy.ActionRequest) policy.Decision {
	g.explained++
	g.request = r
	return g.verdict
}

func TestExplainCustodianProposal_RecordsNothing(t *testing.T) {
	gate := &explainingGate{custodianGateCapture: custodianGateCapture{
		verdict: policy.Decision{Verdict: policy.VerdictQueueApproval,
			RiskTier: policy.RiskModerate, Reason: "approval required"}}}
	exec := New(nil, &config.Config{}, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(gate)
	d := exec.ExplainCustodianProposal(context.Background(), CustodianProposal{
		Feature: "wal", SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '10240MB'",
		TargetObjects: []string{"slot:cdc"}, IsReplica: true})
	if gate.explained != 1 || gate.calls != 0 {
		t.Fatalf("explain calls %d, authorize calls %d; want 1 and 0", gate.explained,
			gate.calls)
	}
	if d.Decision != PolicyDecisionQueueApproval || !d.RequiresApproval ||
		d.RiskTier != string(policy.RiskModerate) {
		t.Fatalf("decision = %+v", d)
	}
	if gate.request.Feature != string(policy.ChangeConfigGUC) || !gate.request.IsReplica ||
		gate.request.Contract == nil || len(gate.request.TargetObjs) != 1 {
		t.Fatalf("explained request = %+v", gate.request)
	}
}

func TestExplainCustodianProposal_FailsClosedWithoutAnExplainer(t *testing.T) {
	gate := &custodianGateCapture{verdict: policy.Decision{Verdict: policy.VerdictExecute}}
	exec := New(nil, &config.Config{}, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(gate)
	d := exec.ExplainCustodianProposal(context.Background(), CustodianProposal{
		Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."t"`,
		TargetObjects: []string{"public.t"}})
	if d.Decision != PolicyDecisionBlocked || d.BlockedReason == "" || gate.calls != 0 {
		t.Fatalf("decision = %+v (authorize calls %d), want blocked without authorizing",
			d, gate.calls)
	}
	var nilExec *Executor
	if d := nilExec.ExplainCustodianProposal(context.Background(),
		CustodianProposal{}); d.Decision != PolicyDecisionBlocked {
		t.Fatalf("nil executor decision = %+v", d)
	}
}
