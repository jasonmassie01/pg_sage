package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Owner decision 2026-10-04 (PR #110): pg_sage executes CREATE STATISTICS.
// It is a typed action in the tuning family (class statistics): reversible
// by DROP STATISTICS IF EXISTS of the created object, SHARE UPDATE
// EXCLUSIVE on the table, risk moderate, and its ANALYZE of the table is
// part of the same action. Only the pg_sage form runs: a sage_stx_ object
// in the table's schema, kinds ndistinct/dependencies/mcv, plain columns
// of one table. It goes through policy.Gate like any other action.

const statsSQL = "CREATE STATISTICS public.sage_stx_orders_ab (dependencies) " +
	"ON a, b FROM public.orders"

const statsRollback = "DROP STATISTICS IF EXISTS public.sage_stx_orders_ab"

func TestValidateAcceptsPgSageStatistics(t *testing.T) {
	for _, sql := range []string{
		statsSQL,
		"CREATE STATISTICS IF NOT EXISTS app.sage_stx_x (ndistinct, mcv) ON a, b, c FROM app.t",
		statsRollback,
		"DROP STATISTICS public.sage_stx_orders_ab;",
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want accepted", sql, err)
		}
	}
}

func TestValidateRefusesOtherStatistics(t *testing.T) {
	for _, sql := range []string{
		"CREATE STATISTICS st_orders ON id, status FROM orders",
		"CREATE STATISTICS public.st_orders ON id, status FROM public.orders",
		"CREATE STATISTICS public.sage_stx_x ON (lower(a)), b FROM public.t",
		"CREATE STATISTICS app.sage_stx_x ON a, b FROM public.t",
		"CREATE STATISTICS sage.sage_stx_x ON a, b FROM sage.findings",
		"CREATE STATISTICS pg_catalog.sage_stx_x ON a, b FROM pg_catalog.pg_class",
		"CREATE STATISTICS public.sage_stx_x (expressions) ON a, b FROM public.t",
		"DROP STATISTICS public.st_orders",
		"DROP STATISTICS public.sage_stx_x CASCADE",
		"DROP STATISTICS sage.sage_stx_x",
		"DROP STATISTICS sage_stx_x",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t; ANALYZE public.t",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t /* c */",
	} {
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL", sql, err)
		}
	}
}

func TestStatisticsActionTypes(t *testing.T) {
	for sql, want := range map[string]string{
		statsSQL:      "create_statistics",
		statsRollback: "revert_created_statistics",
		// not the pg_sage form: no contract, so the gate fails closed
		"CREATE STATISTICS st_orders ON id, status FROM orders": "",
		"DROP STATISTICS public.st_orders":                      "",
	} {
		if got := actionTypeForProposalSQL(sql); got != want {
			t.Errorf("actionTypeForProposalSQL(%q) = %q, want %q", sql, got, want)
		}
	}
	// The action_log label is what the trust ledger reads when no outcome
	// class was recorded; a table or column named "analyze" must not turn
	// a statistics action into an ANALYZE.
	for sql, want := range map[string]string{
		statsSQL: "create_statistics",
		"CREATE STATISTICS public.sage_stx_x ON analyze_at, b FROM public.analyze_log": "create_statistics",
		statsRollback: "drop_statistics",
	} {
		if got := categorizeAction(sql); got != want {
			t.Errorf("categorizeAction(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestCreateStatisticsContract(t *testing.T) {
	c, ok := ContractForActionType("create_statistics")
	if !ok {
		t.Fatal("create_statistics contract missing")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.BaseRiskTier != "moderate" || c.RollbackClass != "reversible" {
		t.Fatalf("risk/rollback = %s/%s, want moderate/reversible", c.BaseRiskTier,
			c.RollbackClass)
	}
	for _, g := range c.Guardrails {
		if isApprovalRequiredGuardrail(g) {
			t.Fatalf("guardrail %q: the earned ledger, not the contract, decides approval", g)
		}
	}
	joined := strings.Join(c.Guardrails, "|")
	for _, want := range []string{"SHARE UPDATE EXCLUSIVE", "lock_timeout", "statement_timeout",
		"sage_stx_"} {
		if !strings.Contains(joined, want) {
			t.Errorf("guardrails %q lack %q", joined, want)
		}
	}
	plan := strings.Join(c.ExecutionPlan, "|")
	if !strings.Contains(plan, "CREATE STATISTICS") || !strings.Contains(plan, "ANALYZE") ||
		!strings.Contains(plan, "one transaction") {
		t.Fatalf("execution plan %q must run CREATE STATISTICS and ANALYZE in one transaction",
			plan)
	}
	if got := changeClassForActionType("create_statistics"); got != string(policy.ChangeAnalyze) {
		t.Fatalf("change class = %q, want analyze (planner statistics)", got)
	}
	gate := policyContract(c)
	if gate.RiskTier != policy.RiskModerate || gate.RollbackClass != policy.RollbackReversible ||
		len(gate.Guardrails) != 0 {
		t.Fatalf("gate contract = %+v", gate)
	}
}

func TestRevertCreatedStatisticsContract(t *testing.T) {
	c, ok := ContractForActionType("revert_created_statistics")
	if !ok {
		t.Fatal("revert_created_statistics contract missing")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.RollbackClass != "reversible" {
		t.Fatalf("rollback class = %q", c.RollbackClass)
	}
	if got := changeClassForActionType("revert_created_statistics"); got !=
		string(policy.ChangeAnalyze) {
		t.Fatalf("change class = %q, want analyze", got)
	}
	// The definition is the action's own SQL: dropping it is derivable,
	// never a non_dup_object_drop refusal.
	if got := policyContract(c).DropKind; got != policy.DropDerivable {
		t.Fatalf("drop kind = %q, want derivable", got)
	}
}

func TestStatisticsRollbackIsDerivedAndMustMatch(t *testing.T) {
	got, err := statisticsRollback(statsSQL, "")
	if err != nil || got != statsRollback {
		t.Fatalf("derived rollback = %q, %v; want %q", got, err, statsRollback)
	}
	got, err = statisticsRollback(statsSQL, "DROP STATISTICS public.sage_stx_orders_ab;")
	if err != nil || got != "DROP STATISTICS public.sage_stx_orders_ab;" {
		t.Fatalf("matching rollback = %q, %v; want it kept", got, err)
	}
	for _, wrong := range []string{
		"DROP STATISTICS IF EXISTS public.sage_stx_other",
		"DROP STATISTICS IF EXISTS app.sage_stx_orders_ab",
		"DROP INDEX CONCURRENTLY IF EXISTS public.sage_stx_orders_ab",
		"DROP STATISTICS IF EXISTS public.sage_stx_orders_ab CASCADE",
	} {
		if _, err := statisticsRollback(statsSQL, wrong); !errors.Is(err, ErrRollbackMismatch) {
			t.Errorf("rollback %q = %v, want ErrRollbackMismatch", wrong, err)
		}
	}
	// Other statements pass through untouched.
	if got, err := statisticsRollback("ANALYZE public.orders", "x"); err != nil || got != "x" {
		t.Fatalf("non-statistics rollback = %q, %v", got, err)
	}
}

func TestFindingRollbackForStatistics(t *testing.T) {
	f := analyzer.Finding{RecommendedSQL: statsSQL, ObjectIdentifier: "public.orders"}
	if err := prepareFindingRollback(&f); err != nil || f.RollbackSQL != statsRollback {
		t.Fatalf("finding rollback = %q, %v; want derived", f.RollbackSQL, err)
	}
	f.RollbackSQL = "DROP STATISTICS IF EXISTS public.sage_stx_other"
	if err := prepareFindingRollback(&f); !errors.Is(err, ErrRollbackMismatch) {
		t.Fatalf("mismatched finding rollback = %v, want ErrRollbackMismatch", err)
	}
}

func TestStatisticsTargets(t *testing.T) {
	if got := operatorLeaseTargets(statsSQL); len(got) != 1 || got[0] != "public.orders" {
		t.Fatalf("lease targets = %v, want the table", got)
	}
	if got := operatorLeaseTargets(statsRollback); len(got) != 1 ||
		got[0] != "public.sage_stx_orders_ab" {
		t.Fatalf("rollback lease targets = %v, want the statistics object", got)
	}
}

// ledgerSpy stands in for the earned ledger: it resolves the request the
// way earned does and answers a fixed level.
type ledgerSpy struct {
	level int
	seen  []policy.AutonomyLimit
}

func (l *ledgerSpy) Limit(_ context.Context, req policy.ActionRequest) (
	policy.AutonomyLimit, error,
) {
	family, class := earned.FamilyForRequest(req)
	limit := policy.AutonomyLimit{Level: l.level, Granted: l.level, Class: string(class),
		Family: string(family)}
	l.seen = append(l.seen, limit)
	return limit, nil
}

func (l *ledgerSpy) Governs(req policy.ActionRequest) bool { return earned.Governs(req) }

func statisticsGate(spy *ledgerSpy) policy.Gate {
	runtime := matrixRuntime("autonomous")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	return policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtime, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now:      func() time.Time { return matrixNow },
		Autonomy: spy,
	})
}

// A self-initiated CREATE STATISTICS is judged by the earned ledger as
// tuning/statistics: its level decides execute, handoff or observe.
func TestStatisticsGatePathFollowsTheLedger(t *testing.T) {
	f := analyzer.Finding{RecommendedSQL: statsSQL, ObjectIdentifier: "public.orders"}
	for level, want := range map[int]policy.Verdict{
		3: policy.VerdictExecute, 2: policy.VerdictQueueApproval, 1: policy.VerdictObserveOnly,
	} {
		spy := &ledgerSpy{level: level}
		req := findingRequest(f, false)
		got := statisticsGate(spy).Authorize(context.Background(), req)
		if got.Verdict != want {
			t.Errorf("level %d: verdict %s/%s (%s), want %s", level, got.Verdict, got.Reason,
				got.Detail, want)
		}
		if len(spy.seen) != 1 || spy.seen[0].Family != string(earned.FamilyTuning) ||
			spy.seen[0].Class != string(earned.ClassStatistics) {
			t.Errorf("level %d: ledger saw %+v, want tuning/statistics", level, spy.seen)
		}
		if kind := policy.BudgetKindFor(req); kind != policy.BudgetPerformance {
			t.Errorf("budget kind = %s, want performance (a tuning change)", kind)
		}
	}
}

// Undoing pg_sage's own statistics is never withheld by the ledger.
func TestStatisticsRollbackBypassesTheLedger(t *testing.T) {
	spy := &ledgerSpy{level: 1}
	req := findingRequest(analyzer.Finding{RecommendedSQL: statsRollback,
		ObjectIdentifier: "public.orders"}, false)
	req.Rollback = true
	got := statisticsGate(spy).Authorize(context.Background(), req)
	if got.Verdict != policy.VerdictExecute {
		t.Fatalf("rollback verdict = %s/%s (%s), want execute", got.Verdict, got.Reason,
			got.Detail)
	}
	if len(spy.seen) != 0 {
		t.Fatalf("ledger consulted for a rollback: %+v", spy.seen)
	}
}

// A policy that does not allow the analyze change class refuses it: the
// class is the integration point, like every other action.
func TestStatisticsRefusedWithoutAnalyzeClass(t *testing.T) {
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	var classes []policy.ChangeClass
	for _, c := range doc.AllowedChangeClasses {
		if c != policy.ChangeAnalyze {
			classes = append(classes, c)
		}
	}
	doc.AllowedChangeClasses = classes
	runtime := matrixRuntime("autonomous")
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtime, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now: func() time.Time { return matrixNow },
	})
	got := gate.Authorize(context.Background(), findingRequest(analyzer.Finding{
		RecommendedSQL: statsSQL, ObjectIdentifier: "public.orders"}, false))
	if got.Verdict != policy.VerdictBlocked || got.Reason != policy.ReasonChangeClassNotAllowed {
		t.Fatalf("verdict = %s/%s, want blocked/change_class_not_allowed", got.Verdict,
			got.Reason)
	}
}
