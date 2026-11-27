package executor

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-01 on real PostgreSQL: contract-less candidates used to fail
// sage.decision's risk_tier CHECK (or, unclassified, ledger validation)
// every cycle; they now record one parked row at tier high.

type contractlessFixture struct {
	pool       *pgxpool.Pool
	ctx        context.Context
	exec       *Executor
	logs       *capturedLog
	databaseID int
	database   string
}

func newContractlessFixture(t *testing.T) *contractlessFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	stamp := time.Now().UnixNano()
	fx := &contractlessFixture{pool: pool, ctx: ctx, logs: &capturedLog{},
		databaseID: 770000 + int(stamp%100000), database: fmt.Sprintf("p01_%d", stamp)}
	t.Cleanup(func() {
		for _, sql := range []string{"DELETE FROM sage.decision WHERE database_id=$1",
			"DELETE FROM sage.policy WHERE database_id=$1"} {
			if _, err := pool.Exec(context.Background(), sql, fx.databaseID); err != nil {
				t.Errorf("cleanup %q: %v", sql, err)
			}
		}
	})
	fx.exec = New(pool, config.DefaultConfig(), time.Time{}, fx.logs.logf)
	fx.exec.WithDatabaseName(fx.database)
	fx.exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	if err := fx.exec.EnableStandingPolicy(ctx, "unattended", &fx.databaseID); err != nil {
		t.Fatalf("EnableStandingPolicy: %v", err)
	}
	return fx
}

type decisionRow struct {
	rows, repeats             int
	tier, reason, contract    string
	feature, verdict, sqlText string
}

func (fx *contractlessFixture) decisionOn(t *testing.T, target string) decisionRow {
	t.Helper()
	var row decisionRow
	err := fx.pool.QueryRow(fx.ctx, `SELECT count(*), COALESCE(sum(repeat_count), 0),
		COALESCE(max(risk_tier), ''), COALESCE(max(reason), ''),
		COALESCE(max(evidence->>'contract'), ''), COALESCE(max(feature), ''),
		COALESCE(max(verdict), ''), COALESCE(max(evidence->>'proposed_sql'), '')
		FROM sage.decision WHERE database_id = $1 AND target_objects = $2::jsonb`,
		fx.databaseID, `["`+target+`"]`).Scan(&row.rows, &row.repeats, &row.tier,
		&row.reason, &row.contract, &row.feature, &row.verdict, &row.sqlText)
	if err != nil {
		t.Fatalf("read decisions on %s: %v", target, err)
	}
	return row
}

// CHECK-02 and CHECK-03: one row (high, no_typed_contract, contract=none)
// folds its repeat, and every evaluation counts as a park.
func TestContractlessFindingParksAndRecordsHigh(t *testing.T) {
	tests := []struct {
		name, sql, feature string
	}{
		{"unclassified truncate", "TRUNCATE public.%s", unclassifiedFeature},
		{"classified but unvalidated", "ALTER TABLE public.%s ADD COLUMN c int; SELECT 1",
			"schema_change"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newContractlessFixture(t)
			target := fmt.Sprintf("p01_t_%d", time.Now().UnixNano())
			finding := analyzer.Finding{ObjectIdentifier: "public." + target,
				RecommendedSQL: fmt.Sprintf(tt.sql, target)}
			for evaluation := 1; evaluation <= 2; evaluation++ {
				decision := fx.exec.evaluateFindingPolicy(fx.ctx, finding, false)
				if decision.Decision != PolicyDecisionParked || decision.RiskTier != "high" ||
					decision.BlockedReason != "no_typed_contract" || decision.DecisionID <= 0 {
					t.Fatalf("evaluation %d: decision = %#v, want a recorded park at high",
						evaluation, decision)
				}
			}
			row := fx.decisionOn(t, "public."+target)
			if row.rows != 1 || row.repeats != 2 || row.tier != "high" ||
				row.reason != "no_typed_contract" || row.contract != "none" ||
				row.verdict != "parked" || row.feature != tt.feature {
				t.Fatalf("decision rows = %#v, want one parked high row seen twice", row)
			}
			if got := parkCount(fx.exec, "no_typed_contract"); got != 2 {
				t.Fatalf("parks counted = %d, want 2", got)
			}
			if got := recordFailureCount(fx.database, "no_typed_contract"); got != 0 {
				t.Fatalf("record failures = %d, want 0", got)
			}
		})
	}
}

// Two evaluations of one candidate racing each other fold into one row.
func TestContractlessParksConcurrentlyFoldIntoOneRow(t *testing.T) {
	fx := newContractlessFixture(t)
	target := fmt.Sprintf("public.p01_c_%d", time.Now().UnixNano())
	finding := analyzer.Finding{ObjectIdentifier: target, RecommendedSQL: "TRUNCATE " + target}
	var wg sync.WaitGroup
	decisions := make([]ActionPolicyDecision, 2)
	for i := range decisions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decisions[i] = fx.exec.evaluateFindingPolicy(fx.ctx, finding, false)
		}(i)
	}
	wg.Wait()
	for i, decision := range decisions {
		if decision.Decision != PolicyDecisionParked || decision.DecisionID <= 0 {
			t.Fatalf("goroutine %d: decision = %#v, want a recorded park", i, decision)
		}
	}
	if decisions[0].DecisionID != decisions[1].DecisionID {
		t.Fatalf("decision ids %d and %d, want one row", decisions[0].DecisionID,
			decisions[1].DecisionID)
	}
	if row := fx.decisionOn(t, target); row.rows != 1 || row.repeats != 2 {
		t.Fatalf("decision rows = %#v, want 1 row with repeat_count 2", row)
	}
}

// A record that PostgreSQL itself refuses (a NUL byte in the evidence)
// fails closed, names the original verdict, is counted and is logged once.
func TestStandingGateReportsRecordFailures(t *testing.T) {
	fx := newContractlessFixture(t)
	target := fmt.Sprintf("public.p01_f_%d", time.Now().UnixNano())
	finding := analyzer.Finding{ObjectIdentifier: target,
		RecommendedSQL: "TRUNCATE " + target + "\x00"}
	for i := 0; i < 2; i++ {
		decision := fx.exec.evaluateFindingPolicy(fx.ctx, finding, false)
		if decision.Decision != PolicyDecisionBlocked ||
			decision.BlockedReason != "policy_unavailable" ||
			!strings.Contains(decision.Detail, "parked/no_typed_contract") {
			t.Fatalf("decision = %#v, want policy_unavailable naming the park", decision)
		}
	}
	if got := recordFailureCount(fx.database, "no_typed_contract"); got != 2 {
		t.Fatalf("record failures = %d, want 2", got)
	}
	if lines := fx.logs.matching("could not record"); len(lines) != 1 {
		t.Fatalf("record-failure log lines = %d, want 1: %v", len(lines), lines)
	}
	if row := fx.decisionOn(t, target); row.rows != 0 {
		t.Fatalf("decision rows = %#v, want none", row)
	}
}

// policyReasons lists every Reason constant the policy package declares,
// read from its source so a new reason is covered without editing this
// test.
func policyReasons(t *testing.T) []policy.Reason {
	t.Helper()
	paths, err := filepath.Glob("../policy/*.go")
	if err != nil || len(paths) == 0 {
		t.Fatalf("list the policy package sources: %v (%d files)", err, len(paths))
	}
	seen := map[policy.Reason]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		collectReasons(file, seen)
	}
	reasons := make([]policy.Reason, 0, len(seen))
	for reason := range seen {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	if len(reasons) < 30 {
		t.Fatalf("found %d Reason constants, want every one (parser drift?)", len(reasons))
	}
	return reasons
}

func collectReasons(file *ast.File, seen map[policy.Reason]bool) {
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || spec.Type == nil || fmt.Sprint(spec.Type) != "Reason" {
			return true
		}
		for _, value := range spec.Values {
			if lit, ok := value.(*ast.BasicLit); ok {
				text, _ := strconv.Unquote(lit.Value)
				seen[policy.Reason(text)] = true
			}
		}
		return true
	})
}

// Every reason, with and without a contract, at the tier the gate stamps
// (high without a contract: see policy's gate_ledger_test.go), passes
// ledger validation and the sage.decision CHECKs through UpsertDecision.
func TestEveryReasonRecordsThroughUpsert(t *testing.T) {
	fx := newContractlessFixture(t)
	service := ledger.NewService(ledger.NewPostgresRepository(fx.pool))
	tiers := []policy.RiskTier{policy.RiskReadOnly, policy.RiskSafe, policy.RiskModerate,
		policy.RiskHigh}
	for i, reason := range policyReasons(t) {
		for _, typed := range []bool{false, true} {
			target := fmt.Sprintf("public.p01_r_%d_%t", i, typed)
			req := policy.ActionRequest{Feature: "vacuum", SQL: "VACUUM " + target,
				TargetObjs: []string{target}}
			decision := policy.Decision{Verdict: policy.VerdictPark, Reason: reason,
				RiskTier: policy.RiskHigh}
			if typed {
				decision.RiskTier = tiers[i%len(tiers)]
				req.Contract = &policy.ActionContract{ActionType: "vacuum_table",
					RiskTier: decision.RiskTier}
			}
			input := ledgerInputAt(&fx.databaseID, 1, req, decision)
			recorded, err := service.RecordDecision(fx.ctx, input)
			if err != nil || recorded.ID <= 0 {
				t.Fatalf("%s typed=%t: record = %#v, %v", reason, typed, recorded, err)
			}
			row := fx.decisionOn(t, target)
			if row.rows != 1 || row.reason != string(reason) ||
				row.tier != string(decision.RiskTier) || (row.contract == "none") == typed {
				t.Fatalf("%s typed=%t: row = %#v", reason, typed, row)
			}
		}
	}
}

// EnableStandingPolicyDocument (the fixed-document gate) wires the same
// record-failure hook as the stored-policy gate: a refused record is
// counted and logged, and the gate fails closed.
func TestFixedDocumentGateReportsRecordFailures(t *testing.T) {
	pool, ctx := requireDB(t)
	logs := &capturedLog{}
	database := fmt.Sprintf("p01_doc_%d", time.Now().UnixNano())
	exec := New(pool, config.DefaultConfig(), time.Time{}, logs.logf)
	exec.WithDatabaseName(database)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	target := fmt.Sprintf("public.p01_d_%d", time.Now().UnixNano())
	finding := analyzer.Finding{ObjectIdentifier: target,
		RecommendedSQL: "TRUNCATE " + target + "\x00"}

	decision := exec.evaluateFindingPolicy(ctx, finding, false)
	if decision.Decision != PolicyDecisionBlocked ||
		decision.BlockedReason != "policy_unavailable" ||
		!strings.Contains(decision.Detail, "could not record the ") {
		t.Fatalf("decision = %#v, want policy_unavailable naming the lost decision", decision)
	}
	var failures int64
	for _, c := range RecordFailureCounts() {
		if c.Database == database {
			failures += c.Count
		}
	}
	if failures != 1 {
		t.Fatalf("record failures for %s = %d, want 1", database, failures)
	}
	if lines := logs.matching("could not record"); len(lines) != 1 {
		t.Fatalf("record-failure log lines = %d, want 1: %v", len(lines), lines)
	}
}
