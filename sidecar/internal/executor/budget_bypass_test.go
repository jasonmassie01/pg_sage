package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Owner decisions 2026-10-03: emergency mitigations bypass the kind
// budgets (recorded as "budget bypass: <why>", charged to no kind), and the
// budget section is serialized across sidecar processes by a transaction-
// scoped advisory lock.

func TestLedgerRecordsBudgetBypassReason(t *testing.T) {
	req := freezeGateRequest("public.t", true)
	detail := "budget bypass: xid runway critical"
	input := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAuthorized, BudgetKind: policy.BudgetBypass, Detail: detail})
	if input.Reason != detail || input.Evidence["budget_kind"] != "bypass" ||
		input.Evidence["gate_reason"] != "authorized" ||
		input.Evidence["budget_detail"] != detail {
		t.Fatalf("ledger input reason=%q evidence=%v, want the bypass recorded", input.Reason,
			input.Evidence)
	}
	queued := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictQueueApproval,
		Reason: policy.ReasonApprovalRequired, BudgetKind: policy.BudgetBypass, Detail: detail})
	if queued.Reason != string(policy.ReasonApprovalRequired) {
		t.Fatalf("withheld bypass reason = %q, want the gate's reason", queued.Reason)
	}
}

// Bypassed executions are charged to no kind; their rows still count
// against the shared rows-rewritten bound.
func TestStandingUsageExcludesBypassedChanges(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := budgetExecutor(t, pool, lifeosLegacyPolicy())
	spendKind(t, pool, ctx, "bypass", 40, "public.frozen", "public.reverted")
	spendKind(t, pool, ctx, "hygiene", 0, "public.analyzed")
	if got := hygieneUsage(t, exec, ctx, "public.idx"); got.SelfInitiatedChangesInWindow != 1 ||
		got.TablesInWindow != 2 || got.RowsRewritten != 80 {
		t.Fatalf("hygiene usage = %+v, want 1 change, 2 tables and the shared 80 rows", got)
	}
	if got := perfUsage(t, exec, ctx, "public.t"); got.SelfInitiatedChangesInWindow != 0 ||
		got.TablesInWindow != 1 {
		t.Fatalf("performance usage = %+v, want nothing charged", got)
	}
}

// freezeGateRequest is a custodian wraparound freeze, with a critical XID
// deadline or none.
func freezeGateRequest(table string, critical bool) policy.ActionRequest {
	contract, _ := ContractForActionType("vacuum_table")
	req := policy.ActionRequest{SQL: "VACUUM (FREEZE) " + table,
		Feature: string(policy.ChangeFreeze), TargetObjs: []string{table},
		Contract: policyContract(contract)}
	if critical {
		req.Deadline = &policy.DeadlineContext{Kind: policy.DeadlineXID,
			Urgency: policy.UrgencyCritical, HardAt: time.Now().Add(6 * time.Hour)}
	}
	return req
}

func zeroBudgets() policy.Document {
	doc := lifeosLegacyPolicy()
	doc.BlastRadius.MaxTablesPerWindow = 0
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 0
	doc.BlastRadius.Hygiene = policy.KindBudget{}
	return doc
}

func TestCriticalFreezeRunsThroughAFullHygieneBudget(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := budgetExecutor(t, pool, zeroBudgets())
	gate := exec.StandingPolicyGate()

	parked := gate.Authorize(ctx, freezeGateRequest("public.fz_plain", false))
	if parked.Verdict != policy.VerdictPark || !strings.Contains(parked.Detail, "hygiene") {
		t.Fatalf("non-critical freeze = %#v, want parked on the hygiene budget", parked)
	}
	critical := gate.Authorize(ctx, freezeGateRequest("public.fz_critical", true))
	if critical.Verdict != policy.VerdictExecute || critical.BudgetKind != policy.BudgetBypass {
		t.Fatalf("critical freeze = %#v, want execute as a budget bypass", critical)
	}
	var reason, kind string
	if err := pool.QueryRow(ctx, `SELECT reason, evidence->>'budget_kind'
		FROM sage.decision WHERE id = $1`, critical.DecisionID).Scan(&reason, &kind); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reason, "budget bypass: ") || kind != "bypass" {
		t.Fatalf("recorded reason=%q kind=%q, want a budget bypass", reason, kind)
	}
}

// An automatic rollback of pg_sage's own change is authorized on a full
// budget; the same statement as a fresh change is parked.
func TestRollbackOfOwnChangeBypassesTheBudget(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := budgetExecutor(t, pool, zeroBudgets())
	finding := analyzer.Finding{ObjectIdentifier: "public.rb", Title: "autovacuum",
		RecommendedSQL: "ALTER TABLE public.rb SET (autovacuum_vacuum_scale_factor = 0.02)"}
	restore := "ALTER TABLE public.rb SET (autovacuum_vacuum_scale_factor = 0.2)"
	fresh := finding
	fresh.RecommendedSQL = restore
	if got := exec.evaluateFindingPolicy(ctx, fresh, false); got.Decision !=
		PolicyDecisionParked {
		t.Fatalf("fresh change on a full budget = %+v, want parked", got)
	}
	if !exec.standingRollbackAuthorizer(finding)(ctx, restore) {
		t.Fatal("rollback of an own change was not authorized on a full budget")
	}
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE verdict = 'execute' AND reason LIKE 'budget bypass: %rollback%'`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("bypass decisions = %d (%v), want the rollback recorded as one", n, err)
	}
}

func TestBudgetLockKeyIsStablePerDatabase(t *testing.T) {
	exec := New(nil, config.DefaultConfig(), time.Time{}, nopLog)
	ns, db := exec.budgetLockKeys()
	if ns != budgetLockNamespace || db != 0 {
		t.Fatalf("standalone lock key = (%d, %d), want (%d, 0)", ns, db, budgetLockNamespace)
	}
	id := 7
	exec.databaseID = &id
	if ns, db := exec.budgetLockKeys(); ns != budgetLockNamespace || db != 7 {
		t.Fatalf("database 7 lock key = (%d, %d)", ns, db)
	}
	if again, _ := exec.budgetLockKeys(); again != ns {
		t.Fatal("lock key is not stable")
	}
}

// secondPool opens another connection pool on the same database, as a
// second sidecar process would.
func secondPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	other, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)
	return other
}

// slowUsageGate is a standing gate over exec's real usage, lock and ledger
// whose usage read pauses, so two processes reliably overlap.
func slowUsageGate(exec *Executor, doc policy.Document) policy.Gate {
	return policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return autonomousRuntime(), nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Usage: func(ctx context.Context, req policy.ActionRequest) (policy.LimitUsage, error) {
			usage, err := exec.standingUsage(ctx, req)
			time.Sleep(60 * time.Millisecond)
			return usage, err
		},
		Serialize: exec.serializeBudget,
		RecordDecisionDetailed: func(ctx context.Context, req policy.ActionRequest,
			decision policy.Decision) (string, int64, error) {
			return exec.recordStandingDecision(ctx, 1, req, decision)
		},
	})
}

func autonomousRuntime() policy.RuntimeState {
	return policy.RuntimeState{ExecutorEnabled: true, TrustLevel: "autonomous",
		ExecutionMode: "auto", Tier3Safe: true, Tier3Moderate: true,
		RampStart: time.Now().Add(-90 * 24 * time.Hour), InConfiguredWindow: true}
}

// raceGates authorizes one new table per racer, alternating between the
// gates, and returns how many were authorized.
func raceGates(t *testing.T, ctx context.Context, gates []policy.Gate, racers int) int {
	t.Helper()
	verdicts := make(chan policy.Verdict, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			table := fmt.Sprintf("public.proc_racer_%d", i)
			verdicts <- gates[i%len(gates)].Authorize(ctx,
				findingRequest(createIndexFinding(table), false)).Verdict
		}(i)
	}
	start.Done()
	done.Wait()
	close(verdicts)
	executed := 0
	for verdict := range verdicts {
		if verdict == policy.VerdictExecute {
			executed++
		}
	}
	return executed
}

// Two sidecar processes (two pools, two gate instances) race for the last
// table of the performance budget on real Postgres: exactly one wins.
func TestLastSlotRaceAcrossTwoSidecars(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	other := secondPool(t, ctx, pool)
	doc := lifeosLegacyPolicy()
	doc.BlastRadius.MaxTablesPerWindow = 3
	first, second := budgetExecutor(t, pool, doc), budgetExecutor(t, other, doc)
	spendKind(t, pool, ctx, "performance", 0, "public.a", "public.b")

	slow := []policy.Gate{slowUsageGate(first, doc), slowUsageGate(second, doc)}
	if got := raceGates(t, ctx, slow, 6); got != 1 {
		t.Fatalf("%d racers across two processes took the last slot, want exactly 1", got)
	}
	// The production gates of both executors take the same lock.
	if _, err := pool.Exec(ctx, `UPDATE sage.decision
		SET evidence = evidence || '{"budget_released": true}'::jsonb
		WHERE verdict = 'execute' AND target_objects::text LIKE '%proc_racer_%'`); err != nil {
		t.Fatalf("release the first race's winner: %v", err)
	}
	production := []policy.Gate{first.StandingPolicyGate(), second.StandingPolicyGate()}
	if got := raceGates(t, ctx, production, 6); got != 1 {
		t.Fatalf("production gates: %d racers took the last slot, want exactly 1", got)
	}
}
