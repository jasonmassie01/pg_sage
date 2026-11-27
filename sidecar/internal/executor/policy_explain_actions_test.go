package executor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// No concurrency test: ExplainActions holds no shared state beyond one
// call (the gate's batch snapshot is local to ExplainBatch).

// protectingBinder is a confirmed fact that protects one relation.
type protectingBinder struct{ protected string }

func (b protectingBinder) Bind(_ context.Context, req policy.ActionRequest) (
	[]policy.FactBinding, error) {
	for _, target := range req.TargetObjs {
		if target == b.protected {
			return []policy.FactBinding{{FactID: 7, Type: "app_managed", Subject: target,
				Summary: target + " is owned by the app's migrations", Object: target}}, nil
		}
	}
	return nil, nil
}

// usageByTarget counts the gate's usage reads per (kind, targets, SQL).
type usageByTarget struct {
	mu    sync.Mutex
	reads map[string]int
}

func (u *usageByTarget) read(_ context.Context, req policy.ActionRequest) (
	policy.LimitUsage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reads == nil {
		u.reads = map[string]int{}
	}
	u.reads[string(policy.BudgetKindFor(req))+"|"+strings.Join(req.TargetObjs, ",")]++
	return policy.LimitUsage{}, nil
}

func usageTestGate(now time.Time, usage *usageByTarget, recorded *int) policy.Gate {
	return policy.NewGate(usageTestConfig(now, usage, recorded))
}

func usageTestConfig(now time.Time, usage *usageByTarget, recorded *int) policy.GateConfig {
	doc := policy.UnattendedProfile()
	return policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{
				ExecutorEnabled: true, TrustLevel: policy.TrustAutonomous,
				ExecutionMode: policy.ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
				RampStart: now.Add(-90 * 24 * time.Hour), InConfiguredWindow: true,
			}, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Usage: usage.read,
		RecordDecision: func(context.Context, policy.ActionRequest, policy.Decision) (
			string, error) {
			*recorded++
			return "recorded", nil
		},
		Now: func() time.Time { return now },
	}
}

// factsTestGate is explainTestGate reading exec's confirmed facts, as the
// standing gate does.
func factsTestGate(now time.Time, exec *Executor, recorded *int) policy.Gate {
	cfg := usageTestConfig(now, &usageByTarget{}, recorded)
	cfg.Facts = executorFacts{exec}
	return policy.NewGate(cfg)
}

func vacuumAction(t *testing.T, table string) ExplainActionRequest {
	t.Helper()
	contract, ok := ContractForActionType("vacuum_table")
	if !ok {
		t.Fatal("missing vacuum_table contract")
	}
	return ExplainActionRequest{Contract: contract, Finding: analyzer.Finding{
		Category: "table_bloat", ObjectType: "table", ObjectIdentifier: table,
		RecommendedSQL: "VACUUM " + table,
	}}
}

// State transition: a confirmed fact that protects public.orders blocks
// its VACUUM, with the gate's reason, and leaves public.items to run.
func TestExplainActionsBlocksTheProtectedTable(t *testing.T) {
	recorded := 0
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(factsTestGate(time.Now(), exec, &recorded))
	exec.WithFactBinder(protectingBinder{protected: "public.orders"})

	got := exec.ExplainActions(context.Background(), []ExplainActionRequest{
		vacuumAction(t, "public.orders"), vacuumAction(t, "public.items"),
	}, false)

	if len(got) != 2 {
		t.Fatalf("decisions = %d, want 2", len(got))
	}
	if got[0].Decision != PolicyDecisionBlocked ||
		got[0].BlockedReason != string(policy.ReasonBoundByFact) ||
		!strings.Contains(got[0].Detail, "public.orders is owned") {
		t.Fatalf("protected table = %+v, want blocked bound_by_fact naming the fact", got[0])
	}
	if got[1].Decision != PolicyDecisionExecute || got[1].BlockedReason != "" {
		t.Fatalf("unprotected table = %+v, want execute", got[1])
	}
	if len(got[1].Guardrails) == 0 || got[1].Provider != "postgres" {
		t.Fatalf("decision lost guardrails or provider: %+v", got[1])
	}
	if recorded != 0 {
		t.Fatalf("explaining recorded %d decisions", recorded)
	}
}

// The family path cannot see the fact: it names no table. This pins why
// candidates with SQL are explained concretely.
func TestExplainFamiliesCannotSeeTheProtectedTable(t *testing.T) {
	recorded := 0
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(factsTestGate(time.Now(), exec, &recorded))
	exec.WithFactBinder(protectingBinder{protected: "public.orders"})
	family := exec.ExplainFamilies(context.Background(),
		[]ActionContract{vacuumAction(t, "public.orders").Contract}, false)
	if len(family) != 1 || family[0].Decision != PolicyDecisionExecute {
		t.Fatalf("family verdict = %+v, want execute (no table to bind)", family)
	}
}

// Boundary (CHECK-08): two tables get their own usage; the same table and
// statement twice share one read.
func TestExplainActionsReadsUsagePerTarget(t *testing.T) {
	recorded := 0
	usage := &usageByTarget{}
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(usageTestGate(time.Now(), usage, &recorded))

	got := exec.ExplainActions(context.Background(), []ExplainActionRequest{
		vacuumAction(t, "public.orders"), vacuumAction(t, "public.items"),
		vacuumAction(t, "public.orders"),
	}, false)

	for i, decision := range got {
		if decision.Decision != PolicyDecisionExecute {
			t.Fatalf("decision %d = %+v, want execute", i, decision)
		}
	}
	want := map[string]int{"hygiene|public.orders": 1, "hygiene|public.items": 1}
	if len(usage.reads) != len(want) {
		t.Fatalf("usage reads = %v, want %v", usage.reads, want)
	}
	for key, n := range want {
		if usage.reads[key] != n {
			t.Fatalf("usage reads = %v, want %v", usage.reads, want)
		}
	}
}

// A finding whose statement changes another table than it names is refused
// as target_mismatch, as the executor would refuse it (executor-08).
func TestExplainActionsBindsTheStatementTarget(t *testing.T) {
	recorded := 0
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(explainTestGate(time.Now(), &recorded))
	request := vacuumAction(t, "public.items")
	request.Finding.RecommendedSQL = "VACUUM public.orders"
	got := exec.ExplainActions(context.Background(), []ExplainActionRequest{request}, false)
	if len(got) != 1 || got[0].Decision != PolicyDecisionBlocked ||
		got[0].BlockedReason != string(policy.ReasonTargetMismatch) {
		t.Fatalf("mismatched target = %+v, want blocked target_mismatch", got)
	}
}

// Error propagation: invalid SQL is refused by the gate's validation, not
// shown as runnable.
func TestExplainActionsRefusesInvalidSQL(t *testing.T) {
	recorded := 0
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(explainTestGate(time.Now(), &recorded))
	request := vacuumAction(t, "public.orders")
	request.Finding.RecommendedSQL = "VACUUM public.orders; DROP TABLE public.orders"
	got := exec.ExplainActions(context.Background(), []ExplainActionRequest{request}, false)
	if len(got) != 1 || got[0].Decision == PolicyDecisionExecute ||
		got[0].Decision == PolicyDecisionQueueApproval ||
		got[0].BlockedReason != string(policy.ReasonNoTypedContract) ||
		!strings.Contains(got[0].Detail, "multi-statement") {
		t.Fatalf("multi-statement = %+v, want withheld as no_typed_contract", got)
	}
}

// Nil/empty and fail closed: no requests, no gate, a nil executor and a
// cancelled ctx.
func TestExplainActionsFailsClosed(t *testing.T) {
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	if got := exec.ExplainActions(context.Background(), nil, false); len(got) != 0 {
		t.Fatalf("empty request list = %+v", got)
	}
	got := exec.ExplainActions(context.Background(),
		[]ExplainActionRequest{vacuumAction(t, "public.orders")}, false)
	if len(got) != 1 || got[0].Decision != PolicyDecisionBlocked ||
		got[0].BlockedReason != reasonNoStandingPolicy {
		t.Fatalf("without gate = %+v, want blocked %q", got, reasonNoStandingPolicy)
	}
	var none *Executor
	got = none.ExplainActions(context.Background(),
		[]ExplainActionRequest{vacuumAction(t, "public.orders")}, false)
	if len(got) != 1 || got[0].Decision != PolicyDecisionBlocked {
		t.Fatalf("nil executor = %+v, want blocked", got)
	}
	recorded := 0
	exec.WithPolicyGate(explainTestGate(time.Now(), &recorded))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got = exec.ExplainActions(ctx, []ExplainActionRequest{vacuumAction(t, "public.orders")}, false)
	if len(got) != 1 || got[0].BlockedReason != string(policy.ReasonPolicyUnavailable) {
		t.Fatalf("cancelled = %+v, want policy_unavailable", got)
	}
}

// ClassifySQL is the contract's type and base tier; no contract is blank.
func TestClassifySQL(t *testing.T) {
	tests := []struct{ sql, actionType, risk string }{
		{"ANALYZE public.orders", "analyze_table", "safe"},
		{"ALTER SYSTEM SET work_mem = '64MB'", "alter_system_guc", "moderate"},
		{"SELECT pg_terminate_backend(42)", "terminate_backend", "high"},
		{"VACUUM FULL public.orders", "", ""},
		{"", "", ""},
		{"SELECT 1", "", ""},
	}
	for _, tt := range tests {
		gotType, gotRisk := ClassifySQL(tt.sql)
		if gotType != tt.actionType || gotRisk != tt.risk {
			t.Errorf("ClassifySQL(%q) = %q/%q, want %q/%q", tt.sql, gotType, gotRisk,
				tt.actionType, tt.risk)
		}
	}
}
