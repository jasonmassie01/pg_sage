package decide

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/policy"
)

// G1-12 composed end to end at the gate: the real decider behind the real
// standing gate, on requests shaped as the MCP intent executor builds
// them (an online-migration expand step for apply_migration, an index
// candidate for request_change), with the principal bound on the context
// as the MCP server binds it.

func composedGate(t *testing.T, cfg Config, trust string) policy.Gate {
	t.Helper()
	doc := policy.StaffedProfile()
	doc.MaintenanceWindows = []string{"always"}
	return policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true, TrustLevel: trust,
					ExecutionMode: policy.ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
					InConfiguredWindow: true, RampStart: time.Now().Add(-90 * 24 * time.Hour)},
				nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Agents: New(cfg).Policy("orders"),
	})
}

func expandStep() policy.ActionRequest {
	return policy.ActionRequest{Feature: "online_migration",
		SQL:        "ALTER TABLE app.orders ADD CONSTRAINT u UNIQUE USING INDEX i",
		TargetObjs: []string{"app.orders"},
		Contract: &policy.ActionContract{ActionType: "online_migration",
			RiskTier: policy.RiskModerate, RollbackClass: policy.RollbackForwardFixOnly}}
}

func indexCandidate() policy.ActionRequest {
	return policy.ActionRequest{Feature: "index",
		SQL:        "CREATE INDEX CONCURRENTLY i ON app.orders (status)",
		TargetObjs: []string{"app.orders"},
		Contract: &policy.ActionContract{ActionType: "create_index",
			RiskTier: policy.RiskSafe, RollbackClass: policy.RollbackReversible}}
}

func agentCtx(tool string) context.Context {
	return policy.WithPrincipalRef(context.Background(),
		policy.PrincipalRef{ID: testID, Tool: tool})
}

func TestG112FrozenPrincipalSchemaIntentsDenied(t *testing.T) {
	p := activePrincipal()
	p.Status, p.FrozenReason = agentguard.StatusFrozen, "canary"
	cfg, _ := fullConfig(p)
	gate := composedGate(t, cfg, policy.TrustAutonomous)
	for tool, req := range map[string]policy.ActionRequest{
		"apply_migration": expandStep(), "request_change": indexCandidate()} {
		got := gate.Authorize(agentCtx(tool), req)
		if got.Verdict != policy.VerdictBlocked ||
			got.Reason != policy.Reason(agentguard.ReasonFrozen) {
			t.Fatalf("%s by a frozen principal = %+v, want blocked agent_frozen", tool, got)
		}
	}
}

func TestG112TaintedPrincipalSchemaIntentsQueued(t *testing.T) {
	p := activePrincipal()
	p.Tainted = true
	cfg, _ := fullConfig(p)
	gate := composedGate(t, cfg, policy.TrustAutonomous)
	for tool, req := range map[string]policy.ActionRequest{
		"apply_migration": expandStep(), "request_change": indexCandidate()} {
		got := gate.Authorize(agentCtx(tool), req)
		if got.Verdict != policy.VerdictQueueApproval ||
			got.Reason != policy.ReasonApprovalRequired {
			t.Fatalf("%s by a tainted principal = %+v, want queue_approval", tool, got)
		}
	}
}

func TestG112ActivePrincipalNeverAutoExecutes(t *testing.T) {
	// Behaviour change (§6.2.6): what pg_sage would run under its own
	// earned trust queues for a person when an agent asks.
	cfg, _ := fullConfig(activePrincipal())
	gate := composedGate(t, cfg, policy.TrustAutonomous)
	got := gate.Authorize(agentCtx("request_change"), indexCandidate())
	if got.Verdict != policy.VerdictQueueApproval {
		t.Fatalf("agent index request = %+v, want queue_approval", got)
	}
	own := gate.Authorize(context.Background(), indexCandidate())
	if own.Verdict != policy.VerdictExecute {
		t.Fatalf("pg_sage's own = %+v, want execute (the contrast)", own)
	}
}

func TestG1ActingNeedsAdvisory(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	gate := composedGate(t, cfg, policy.TrustObservation)
	got := gate.Authorize(agentCtx("request_change"), indexCandidate())
	if got.Verdict != policy.VerdictObserveOnly ||
		got.Reason != policy.ReasonAgentProposalRecorded {
		t.Fatalf("observation = %+v, want an L1 proposal record", got)
	}
}

func TestG1NarrowingWorksUnderObservationForFrozenAgent(t *testing.T) {
	p := activePrincipal()
	p.Status = agentguard.StatusFrozen
	cfg, _ := fullConfig(p)
	gate := composedGate(t, cfg, policy.TrustObservation)
	req := policy.ActionRequest{Feature: string(policy.ChangeAgentAccess),
		InternalControl: true, TargetObjs: []string{"grant:g1"},
		Contract: &policy.ActionContract{ActionType: "guard_freeze",
			RiskTier: policy.RiskSafe, RollbackClass: policy.RollbackReversible,
			Narrowing: true}}
	got := gate.Authorize(agentCtx("agent_query"), req)
	if got.Verdict != policy.VerdictExecute {
		t.Fatalf("narrowing in a frozen agent's context = %+v, want execute", got)
	}
}
