package decide

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The policy.AgentDecider adapter (§6.2.1): the gate's A4 step for one
// executor's database.

func migrationAction(tool string) policy.ActionRequest {
	return policy.ActionRequest{
		Contract: &policy.ActionContract{ActionType: "online_migration",
			RiskTier: policy.RiskModerate, RollbackClass: policy.RollbackForwardFixOnly},
		SQL:       "ALTER TABLE app.orders ALTER COLUMN note SET NOT NULL",
		Feature:   "online_migration",
		Principal: &policy.PrincipalRef{ID: testID, Tool: tool, TaskID: "t-9"},
	}
}

func TestPolicyAdapterMapsRequest(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	dec, level, stop := New(cfg).Policy("orders").Decide(context.Background(),
		migrationAction("apply_migration"))
	if stop || level != 2 || dec.Verdict != "" {
		t.Fatalf("adapter = (%+v, %d, %v), want continue at L2", dec, level, stop)
	}
	got := toRequest("orders", migrationAction("apply_migration"))
	if got.PrincipalID != testID || got.Tool != "apply_migration" ||
		got.Kind != agentguard.ToolPropose || got.Capability != CapDDLLocking ||
		got.Database != "orders" || got.TaskID != "t-9" || got.Narrowing ||
		got.OperatorApproved {
		t.Fatalf("mapped request = %+v", got)
	}
}

func TestPolicyAdapterExplicitCapabilityWins(t *testing.T) {
	req := migrationAction("apply_migration")
	req.CapabilityClass = string(CapDDLAdditive)
	req.OperatorApproved = true
	seen := []Request{toRequest("orders", req)}
	if seen[0].Capability != CapDDLAdditive || !seen[0].OperatorApproved {
		t.Fatalf("mapped = %+v", seen[0])
	}
}

func TestPolicyAdapterReadToolNeverActsAboveL2(t *testing.T) {
	// A read-scope tool that reaches the gate (ask_sage proposing an
	// action) is a proposal, never an L3 read.
	cfg, _ := fullConfig(activePrincipal())
	req := migrationAction("ask_sage")
	seen := []Request{toRequest("orders", req)}
	_, level, stop := New(cfg).Policy("orders").Decide(context.Background(), req)
	if stop || level != 2 || seen[0].Kind != agentguard.ToolPropose {
		t.Fatalf("level=%d stop=%v kind=%s, want a propose-kind request at L2",
			level, stop, seen[0].Kind)
	}
}

func TestPolicyAdapterMapsDenials(t *testing.T) {
	p := activePrincipal()
	p.Status = agentguard.StatusFrozen
	cfg, _ := fullConfig(p)
	dec, level, stop := New(cfg).Policy("orders").Decide(context.Background(),
		migrationAction("request_change"))
	if !stop || level != 0 || dec.Verdict != policy.VerdictBlocked ||
		dec.Reason != policy.Reason(agentguard.ReasonFrozen) || dec.Detail == "" {
		t.Fatalf("frozen = (%+v, %d, %v), want blocked agent_frozen", dec, level, stop)
	}
}

func TestPolicyAdapterMapsPark(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.MaxRequestsPerHour = 1
	adapter := New(cfg).Policy("orders")
	adapter.Decide(context.Background(), migrationAction("apply_migration"))
	dec, _, stop := adapter.Decide(context.Background(), migrationAction("apply_migration"))
	if !stop || dec.Verdict != policy.VerdictPark || dec.Reason != policy.Reason(ReasonRate) {
		t.Fatalf("rate = %+v stop=%v, want park agent_rate", dec, stop)
	}
	if dec.Detail == "" {
		t.Fatal("a park names its retry_after")
	}
}

func TestPolicyAdapterWithoutPrincipalFailsClosed(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	req := migrationAction("apply_migration")
	req.Principal = nil
	dec, _, stop := New(cfg).Policy("orders").Decide(context.Background(), req)
	if !stop || dec.Verdict != policy.VerdictBlocked {
		t.Fatalf("no principal = %+v stop=%v, want blocked", dec, stop)
	}
}

func TestPolicyAdapterNarrowing(t *testing.T) {
	p := activePrincipal()
	p.Status = agentguard.StatusFrozen
	cfg, _ := fullConfig(p)
	req := migrationAction("agent_revoke")
	req.Contract.Narrowing = true
	dec, level, stop := New(cfg).Policy("orders").Decide(context.Background(), req)
	if stop || level != 3 || dec.Verdict != "" {
		t.Fatalf("narrowing = (%+v, %d, %v), want continue", dec, level, stop)
	}
}

func TestPolicyAdapterEnvironmentIsTheExecutorsDatabase(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Environments = envByName{"orders": envbind.EnvProd, "scratch": envbind.EnvDev}
	d := New(cfg)
	req := migrationAction("optimize_query")
	req.SQL = "CREATE INDEX CONCURRENTLY i ON app.t (c)"
	if _, _, stop := d.Policy("scratch").Decide(context.Background(), req); stop {
		t.Fatal("scratch is dev: within the stage ceiling")
	}
	dec, _, stop := d.Policy("orders").Decide(context.Background(), req)
	if !stop || dec.Reason != policy.Reason(ReasonEnvCeiling) {
		t.Fatalf("orders (prod) = %+v, want agent_env_ceiling", dec)
	}
}

type envByName map[string]envbind.Env

func (e envByName) Environment(_ context.Context, db string) (envbind.Binding, error) {
	env, ok := e[db]
	if !ok {
		env = envbind.EnvProd
	}
	return envbind.Binding{Env: env}, nil
}
