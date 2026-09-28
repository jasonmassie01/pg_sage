package policy

import (
	"context"
	"testing"
)

// ExplainBatch evaluates many requests (UI readiness for ~25 action
// families) against one snapshot: runtime, policy and usage are each read
// once, and nothing is recorded.
func TestExplainBatchReadsStateOnceAndMatchesExplain(t *testing.T) {
	var calls []string
	gate := newTestGate(t, gateFixture{calls: &calls})
	safe := validIndexRequest()
	safe.ExplainFamily, safe.SQL = true, ""
	guarded := validIndexRequest()
	guarded.Contract.Guardrails = []Guardrail{GuardrailApprovalRequired}
	unsupported := validIndexRequest()
	unsupported.Contract.ProviderSupport = []string{"rds"}

	batch, ok := gate.(BatchExplainer)
	if !ok {
		t.Fatalf("%T does not implement BatchExplainer", gate)
	}
	got := batch.ExplainBatch(context.Background(), []ActionRequest{safe, guarded, unsupported})

	if len(got) != 3 {
		t.Fatalf("decisions = %d, want 3", len(got))
	}
	want := []Reason{ReasonAuthorized, ReasonApprovalRequired, ReasonProviderUnsupported}
	for i, reason := range want {
		if got[i].Reason != reason {
			t.Errorf("decision %d = %#v, want reason %s", i, got[i], reason)
		}
		if got[i].EvidenceID != "" {
			t.Errorf("decision %d carried evidence id %q", i, got[i].EvidenceID)
		}
	}
	counts := map[string]int{}
	for _, call := range calls {
		counts[call]++
	}
	if counts["runtime"] != 1 || counts["policy"] != 1 || counts["usage"] > 1 {
		t.Fatalf("state reads = %v, want runtime/policy once, usage at most once", counts)
	}
	if counts["evidence"] != 0 {
		t.Fatalf("batch recorded %d decisions", counts["evidence"])
	}
}

func TestExplainBatchEmpty(t *testing.T) {
	var calls []string
	gate := newTestGate(t, gateFixture{calls: &calls})
	got := gate.(BatchExplainer).ExplainBatch(context.Background(), nil)
	if len(got) != 0 || len(calls) != 0 {
		t.Fatalf("empty batch: decisions=%v calls=%v", got, calls)
	}
}

// Read-only diagnostics mutate nothing, so they carry no change class and
// the standing policy's change-class allowlist does not apply to them.
func TestReadOnlyRequestNeedsNoChangeClass(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	req := ActionRequest{Contract: &ActionContract{
		ActionType: "diagnose_standby_conflicts", RiskTier: RiskReadOnly,
	}, ExplainFamily: true}
	got := gate.(Explainer).Explain(context.Background(), req)
	if got.Verdict != VerdictExecute {
		t.Fatalf("read-only diagnostic = %#v, want execute", got)
	}
	req.Contract.RiskTier = RiskSafe
	if got := gate.(Explainer).Explain(context.Background(), req); got.Reason !=
		ReasonChangeClassNotAllowed {
		t.Fatalf("classless safe request = %#v, want change_class_not_allowed", got)
	}
}
