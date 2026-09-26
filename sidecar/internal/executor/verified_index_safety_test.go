package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Regression tests for G4-B08, G4-B09, G3-B05, G3-B20 (executor side) and
// Codex C15 (executor side): verified-index reverts must be bound to the
// exact index this action created.

func verifiedFinding(create, rollback string) analyzer.Finding {
	return analyzer.Finding{
		Category: "query_optimization", ObjectIdentifier: "public.orders",
		RecommendedSQL: create, RollbackSQL: rollback,
		Detail: map[string]any{"queryids": []int64{11}},
	}
}

func TestVerifiedActionRejectsUnsafeCreateForms(t *testing.T) {
	tests := map[string]analyzer.Finding{
		"if not exists": verifiedFinding(
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_o ON public.orders (a)",
			"DROP INDEX CONCURRENTLY IF EXISTS public.idx_o"),
		"unique": verifiedFinding(
			"CREATE UNIQUE INDEX CONCURRENTLY idx_o ON public.orders (a)",
			"DROP INDEX CONCURRENTLY IF EXISTS public.idx_o"),
		"rollback drops another index": verifiedFinding(
			"CREATE INDEX CONCURRENTLY idx_new ON public.orders (a, b)",
			"DROP INDEX CONCURRENTLY IF EXISTS public.idx_existing"),
		"rollback drops same name in other schema": verifiedFinding(
			"CREATE INDEX CONCURRENTLY idx_new ON public.orders (a, b)",
			"DROP INDEX CONCURRENTLY IF EXISTS sales.idx_new"),
	}
	for name, finding := range tests {
		if _, err := verifiedActionForFinding(finding); err == nil {
			t.Fatalf("%s: verifiedActionForFinding accepted unsafe action", name)
		}
	}
	if _, err := verifiedActionForFinding(verifiedFinding(
		"CREATE INDEX CONCURRENTLY idx_new ON public.orders (a, b)",
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx_new",
	)); err != nil {
		t.Fatalf("matching rollback rejected: %v", err)
	}
}

func verifiedTable(t *testing.T, schema string) (string, *Executor, context.Context) {
	t.Helper()
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("vidx_%d", time.Now().UnixNano())
	for _, sql := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + schema,
		"CREATE TABLE " + schema + "." + table + " (a int, b int)",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+schema+"."+table)
	})
	exec := New(pool, config.DefaultConfig(), nil, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	return table, exec, ctx
}

func TestPrepareVerifiedIndexRefusesExistingName(t *testing.T) {
	table, exec, ctx := verifiedTable(t, "public")
	name := table + "_pre"
	if _, err := exec.pool.Exec(ctx, "CREATE INDEX "+name+" ON public."+table+" (a, b)"); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	finding := verifiedFinding(
		"CREATE INDEX CONCURRENTLY "+name+" ON public."+table+" (a)",
		"DROP INDEX CONCURRENTLY IF EXISTS public."+name)
	action, err := verifiedActionForFinding(finding)
	if err != nil {
		t.Fatalf("verifiedActionForFinding: %v", err)
	}

	err = exec.prepareVerifiedIndex(ctx, &action)

	if err == nil {
		t.Fatal("verified create proceeded although the index name already exists")
	}
}

func TestPrepareVerifiedIndexQualifiesNameOutsideSearchPath(t *testing.T) {
	table, exec, ctx := verifiedTable(t, "sage_vidx_sales")
	name := table + "_a"
	action, err := verifiedActionForFinding(verifiedFinding(
		"CREATE INDEX CONCURRENTLY "+name+" ON sage_vidx_sales."+table+" (a)",
		"DROP INDEX CONCURRENTLY IF EXISTS sage_vidx_sales."+name))
	if err != nil {
		t.Fatalf("verifiedActionForFinding: %v", err)
	}

	if err := exec.prepareVerifiedIndex(ctx, &action); err != nil {
		t.Fatalf("prepareVerifiedIndex: %v", err)
	}

	want := `"sage_vidx_sales"."` + name + `"`
	if action.IndexName != want {
		t.Fatalf("IndexName = %q, want %q", action.IndexName, want)
	}
	if action.RollbackSQL != "DROP INDEX CONCURRENTLY IF EXISTS "+want {
		t.Fatalf("RollbackSQL = %q, want deterministic drop of %s", action.RollbackSQL, want)
	}
}

type allowGate struct{}

func (allowGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate}
}

func revertFixture(t *testing.T, recordedOID string) (*Executor, int64, string, string) {
	t.Helper()
	table, exec, ctx := verifiedTable(t, "public")
	name := table + "_rv"
	if _, err := exec.pool.Exec(ctx, "CREATE INDEX "+name+" ON public."+table+" (a)"); err != nil {
		t.Fatalf("create index: %v", err)
	}
	qualified := `"public"."` + name + `"`
	if recordedOID == "" {
		if err := exec.pool.QueryRow(ctx, "SELECT to_regclass($1)::oid::text", qualified).
			Scan(&recordedOID); err != nil {
			t.Fatalf("lookup oid: %v", err)
		}
	}
	rollback := "DROP INDEX CONCURRENTLY IF EXISTS " + qualified
	var id int64
	err := exec.pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, before_state)
		VALUES ('create_index', 'CREATE INDEX CONCURRENTLY x ON public.t (a)', $1,
		'monitoring', jsonb_build_object('created_index_oid', $2::bigint,
		'created_index', $3::text)) RETURNING id`, rollback, recordedOID, qualified).Scan(&id)
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = exec.pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	exec.WithPolicyGate(allowGate{})
	return exec, id, rollback, qualified
}

func indexExists(t *testing.T, exec *Executor, qualified string) bool {
	t.Helper()
	var exists bool
	if err := exec.pool.QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", qualified).Scan(&exists); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	return exists
}

func TestRevertRefusesIndexWithDifferentIdentity(t *testing.T) {
	exec, id, rollback, qualified := revertFixture(t, "1")
	verdict := verify.Verdict{Revert: true, Status: "reverted", Reason: "no_gain"}

	err := (&executorIndexActions{exec: exec}).Revert(context.Background(), id, rollback, verdict)

	if err == nil {
		t.Fatal("revert succeeded against a different index identity")
	}
	if !indexExists(t, exec, qualified) {
		t.Fatal("revert dropped an index this action did not create")
	}
}

func TestRevertDropsOwnIndexAndCompletesVerification(t *testing.T) {
	exec, id, rollback, qualified := revertFixture(t, "")
	verdict := verify.Verdict{Revert: true, Status: "reverted", Reason: "no_gain"}

	if err := (&executorIndexActions{exec: exec}).Revert(
		context.Background(), id, rollback, verdict); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if indexExists(t, exec, qualified) {
		t.Fatal("own index survived revert")
	}
}

func TestFailedRevertKeepsVerificationRetryable(t *testing.T) {
	exec, id, rollback, _ := revertFixture(t, "")
	ctx := context.Background()
	var decisionID int64
	if err := exec.pool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id, policy_version)
		VALUES ('index','index','execute','moderate','authorized',$1,1) RETURNING id`,
		fmt.Sprintf("ev_revert_%d", id)).Scan(&decisionID); err != nil {
		t.Fatalf("decision: %v", err)
	}
	if _, err := exec.pool.Exec(ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, reason)
		VALUES ($1, $2, '{}', '{}', 1, now(), now(), 'revert', 'no_gain')`,
		decisionID, id); err != nil {
		t.Fatalf("verification: %v", err)
	}
	exec.emergencyStopFn = func(context.Context) bool { return true }

	err := (&executorIndexActions{exec: exec}).Revert(ctx, id, rollback,
		verify.Verdict{Revert: true, Status: "reverted", Reason: "no_gain"})

	if err == nil {
		t.Fatal("revert under emergency stop reported success")
	}
	var completed bool
	if err := exec.pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL
		FROM sage.verification WHERE action_log_id=$1`, id).Scan(&completed); err != nil {
		t.Fatalf("read verification: %v", err)
	}
	if completed {
		t.Fatal("withheld revert marked the verification complete")
	}
}

func TestVerifiedCreateRequiresPreparedIdentity(t *testing.T) {
	if _, err := verifiedActionForFinding(verifiedFinding(
		"CREATE INDEX CONCURRENTLY idx_new ON public.orders (a, b)", "",
	)); !errors.Is(err, ErrVerificationUnavailable) {
		t.Fatalf("missing rollback accepted: %v", err)
	}
}
