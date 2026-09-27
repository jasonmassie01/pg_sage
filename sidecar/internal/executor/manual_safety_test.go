package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Regression tests for G6-B03, G4-B27, G4-B29, G4-B30, G4-B20, G4-B32 and
// SURF-03 / G2-B11 (action_log.database_id stamping).

func manualFixture(t *testing.T, sql string) (*pgxpool.Pool, string, int) {
	t.Helper()
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("manual_safety_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+" (a int, b int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	sql = strings.ReplaceAll(sql, "{table}", table)
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql)
		VALUES ('manual_safety', 'warning', 'index', $1, 'manual safety', '{}', 'rec', $2)
		RETURNING id`, "public."+table, sql).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, "DELETE FROM sage.action_log WHERE finding_id=$1", findingID)
		_, _ = pool.Exec(cleanup, "DELETE FROM sage.findings WHERE id=$1", findingID)
		_, _ = pool.Exec(cleanup, "DROP TABLE IF EXISTS public."+table)
	})
	return pool, table, findingID
}

func manualExecutor(pool *pgxpool.Pool) *Executor {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Trust.RollbackWindowMinutes = 1
	exec := New(pool, cfg, nil, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	return exec
}

func TestExecuteManualSurvivesCallerCancellation(t *testing.T) {
	sql := "CREATE INDEX CONCURRENTLY {table}_a ON public.{table} (a)"
	pool, table, findingID := manualFixture(t, sql)
	exec := manualExecutor(pool)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	withTestStandingGate(exec)
	actionID, err := exec.ExecuteManual(cancelled, findingID,
		strings.ReplaceAll(sql, "{table}", table), "", nil)

	if err != nil || actionID <= 0 {
		t.Fatalf("ExecuteManual on a disconnected request = %d, %v", actionID, err)
	}
	var valid bool
	if err := pool.QueryRow(context.Background(), `SELECT indisvalid FROM pg_index
		WHERE indexrelid = to_regclass($1)`, "public."+table+"_a").Scan(&valid); err != nil ||
		!valid {
		t.Fatalf("index valid=%v err=%v, want a completed valid index", valid, err)
	}
}

func TestExecuteManualRecordsDecisionAndDatabase(t *testing.T) {
	sql := "ANALYZE public.{table}"
	pool, table, findingID := manualFixture(t, sql)
	exec := manualExecutor(pool)
	databaseID := 4242
	exec.databaseID = &databaseID

	withTestStandingGate(exec)
	actionID, err := exec.ExecuteManual(context.Background(), findingID,
		strings.ReplaceAll(sql, "{table}", table), "", nil)
	if err != nil {
		t.Fatalf("ExecuteManual: %v", err)
	}

	var decisionID, gotDatabase *int64
	var verified bool
	if err := pool.QueryRow(context.Background(), `SELECT al.decision_id, al.database_id,
		EXISTS(SELECT 1 FROM sage.verification v WHERE v.action_log_id = al.id)
		FROM sage.action_log al WHERE al.id=$1`, actionID).
		Scan(&decisionID, &gotDatabase, &verified); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if decisionID == nil || !verified {
		t.Fatalf("operator action decision=%v verified=%v, want both", decisionID, verified)
	}
	if gotDatabase == nil || *gotDatabase != 4242 {
		t.Fatalf("action database_id = %v, want 4242", gotDatabase)
	}
}

func TestExecuteManualKeepsUnrelatedInvalidIndexes(t *testing.T) {
	sql := "CREATE INDEX CONCURRENTLY {table}_new ON public.{table} (a)"
	pool, table, findingID := manualFixture(t, sql)
	ctx := context.Background()
	other := table + "_user_idx"
	for _, statement := range []string{
		"CREATE INDEX " + other + " ON public." + table + " (a, b)",
		`UPDATE pg_index SET indisvalid = false WHERE indexrelid = to_regclass('public.` +
			other + `')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec := manualExecutor(pool)

	withTestStandingGate(exec)
	if _, err := exec.ExecuteManual(ctx, findingID,
		strings.ReplaceAll(sql, "{table}", table), "", nil); err != nil {
		t.Fatalf("ExecuteManual: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL",
		"public."+other).Scan(&exists); err != nil || !exists {
		t.Fatalf("approved CREATE INDEX dropped unrelated index %s (exists=%v err=%v)",
			other, exists, err)
	}
}

func TestExecuteManualWaitsForSharedDDLSlot(t *testing.T) {
	exec := manualExecutor(nil)
	for i := 0; i < cap(exec.ddlSem); i++ {
		exec.ddlSem <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()

	_, err := exec.ExecuteManual(ctx, 1, "ANALYZE public.orders", "", nil)

	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("ExecuteManual with no DDL slot = %v after %s", err, time.Since(started))
	}
	if !strings.Contains(err.Error(), "DDL slot") {
		t.Fatalf("error = %v, want DDL slot contention", err)
	}
}

func TestCustodianBacksOffAfterRepeatedFailures(t *testing.T) {
	pool, ctx := requireDB(t)
	sql := fmt.Sprintf(`VACUUM (FREEZE) "public"."backoff_%d"`, time.Now().UnixNano())
	for i := 0; i < custodianMaxFailures; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log
			(action_type, sql_executed, outcome) VALUES ('vacuum', $1, 'failed')`, sql); err != nil {
			t.Fatalf("insert failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE sql_executed=$1", sql)
	})
	exec := manualExecutor(pool)
	gate := &custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
	}}
	exec.WithPolicyGate(gate)

	err := exec.SubmitCustodianProposal(ctx, CustodianProposal{
		Feature: "freeze", SQL: sql, TargetObjects: []string{"public.backoff"},
	})

	if err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("SubmitCustodianProposal after repeated failures = %v, want backoff", err)
	}
	if gate.calls != 0 {
		t.Fatalf("backed-off proposal still consulted the gate %d times", gate.calls)
	}
}

type sequenceGate struct {
	verdicts []policy.Verdict
	calls    int
}

func (g *sequenceGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	verdict := g.verdicts[len(g.verdicts)-1]
	if g.calls < len(g.verdicts) {
		verdict = g.verdicts[g.calls]
	}
	g.calls++
	return policy.Decision{Verdict: verdict, RiskTier: policy.RiskModerate,
		Reason: policy.ReasonEmergencyStop}
}

func TestVerifiedIndexProposalReauthorizesAfterAdmission(t *testing.T) {
	exec := manualExecutor(nil)
	gate := &sequenceGate{verdicts: []policy.Verdict{
		policy.VerdictExecute, policy.VerdictBlocked,
	}}
	exec.WithPolicyGate(gate)
	actions := &fakeVerifiedIndexActions{}
	exec.indexVerification = newVerifiedIndexLifecycle(
		&fakeIndexVerifier{admission: verify.Admission{OK: true}}, actions)

	err := exec.SubmitVerifiedIndexProposal(context.Background(), CustodianProposal{
		Feature: "fk_index", TargetObjects: []string{"public.orders"},
		SQL: "CREATE INDEX CONCURRENTLY orders_fk_idx ON public.orders (customer_id)",
	}, "DROP INDEX CONCURRENTLY IF EXISTS public.orders_fk_idx", []int64{7})

	if err == nil || gate.calls < 2 {
		t.Fatalf("SubmitVerifiedIndexProposal = %v after %d authorizations", err, gate.calls)
	}
}
