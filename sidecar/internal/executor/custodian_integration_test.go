package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

func TestCustodianSyntheticActionExecutesAndTriggersSchemaGuard(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "custodian_schema_probe"
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+" (id bigint)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	decisionID := recordCustodianDecision(t, ctx, pool, "autovacuum_tuning", table)
	cfg := config.DefaultConfig()
	exec := New(pool, cfg, nil, zeroTime(), func(string, string, ...any) {})
	gate := &custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
		DecisionID: decisionID,
	}}
	exec.WithPolicyGate(gate)
	hookCalls := 0
	exec.WithPostDDLHook(func(context.Context) error {
		hookCalls++
		return nil
	})
	sql := "ALTER TABLE public." + table +
		" SET (autovacuum_vacuum_scale_factor = 0.02)"
	if err := exec.SubmitCustodianProposal(ctx, CustodianProposal{
		Feature: "autovacuum_tuning", SQL: sql,
		TargetObjects: []string{"public." + table},
	}); err != nil {
		t.Fatalf("SubmitCustodianProposal: %v", err)
	}
	if hookCalls != 1 || gate.calls != 2 {
		t.Fatalf("hook calls=%d authorizations=%d, want 1/2", hookCalls, gate.calls)
	}
	var findingID *int64
	var criterionKind, verdict string
	if err := pool.QueryRow(ctx, `SELECT al.finding_id,
		COALESCE(v.criterion->>'kind',''), COALESCE(v.verdict,'')
		FROM sage.action_log al LEFT JOIN sage.verification v ON v.action_log_id=al.id
		WHERE al.sql_executed=$1 ORDER BY al.id DESC LIMIT 1`, sql).
		Scan(&findingID, &criterionKind, &verdict); err != nil {
		t.Fatalf("read synthetic action: %v", err)
	}
	if findingID != nil {
		t.Fatalf("synthetic finding_id = %v, want NULL", *findingID)
	}
	if criterionKind != "custodian_autovacuum" || verdict != "success" {
		t.Fatalf("custodian verification = %q/%q", criterionKind, verdict)
	}
}

func TestCustodianDDLHonorsConflictingChangeLease(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "custodian_lease_probe"
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+" (id bigint)"); err != nil {
		t.Fatalf("create lease probe: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	decisionID := recordCustodianDecision(t, ctx, pool, "fk_index", table)
	objects, err := policy.NormalizeTargetObjects([]string{"public." + table})
	if err != nil {
		t.Fatalf("NormalizeTargetObjects: %v", err)
	}
	manager := policy.NewPostgresLeaseManager(pool, nil, decisionID, time.Minute)
	leaseID, err := manager.AcquireLease(ctx, "test", objects, "conflict")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	t.Cleanup(func() { _ = manager.ReleaseLease(context.Background(), leaseID) })
	cfg := config.DefaultConfig()
	exec := New(pool, cfg, nil, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
		DecisionID: decisionID,
	}})
	err = exec.SubmitCustodianProposal(ctx, CustodianProposal{
		Feature:       "autovacuum_tuning",
		SQL:           "ALTER TABLE public." + table + " SET (autovacuum_enabled = true)",
		TargetObjects: []string{"public." + table},
	})
	if !errors.Is(err, policy.ErrLeaseConflict) {
		t.Fatalf("conflicting custodian DDL error = %v", err)
	}
}

func recordCustodianDecision(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, feature, table string,
) int64 {
	t.Helper()
	decision, err := ledger.NewService(ledger.NewPostgresRepository(pool)).RecordDecision(
		ctx, ledger.DecisionInput{
			Feature: feature, Intent: feature, Evidence: map[string]any{},
			Verdict: ledger.VerdictExecute, Reason: "test authorization",
			RiskTier: "safe", PolicyVersion: 1,
			TargetObjects: []string{"public." + table},
		},
	)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	return decision.ID
}
