package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// config-01 on real PostgreSQL: a live ceiling downgrade refuses the next
// unattended SAFE ANALYZE (recorded in the decision ledger with the tier
// ceiling's reason), and a raised rollback cooldown is what the apply path
// checks, not the startup value.

func TestTrustCeilingDowngradeRefusesQueuedAnalyzeOnPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	e := New(pool, windowedAutonomousConfig(), time.Now().Add(-60*24*time.Hour),
		noopExecLog)
	e.WithEmergencyStopCheck(func(context.Context) bool { return false })
	e.SetExecutionMode("auto")
	e.EnableStandingPolicyDocument(unlimitedWindowPolicy(), nil)
	table := "public.trust_ceiling_" + time.Now().UTC().Format("150405000000")
	for _, sql := range []string{
		"CREATE TABLE " + table + " (id int)",
		"INSERT INTO " + table + " SELECT generate_series(1, 50)",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	f := analyzer.Finding{Category: "stale_statistics", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: table, Title: "stale statistics",
		Recommendation: "analyze it", RecommendedSQL: "ANALYZE " + table,
		Detail: map[string]any{"n_mod_since_analyze": 50}}
	if got := e.evaluateFindingPolicy(ctx, f, false); got.Decision != PolicyDecisionExecute {
		t.Fatalf("before downgrade: %s (%s), want execute", got.Decision, got.BlockedReason)
	}
	lowered := ceilingFromTrust(windowedAutonomousConfig().Trust)
	lowered.Tier3Safe = false
	e.SetTrustCeiling(&lowered)
	got := e.evaluateFindingPolicy(ctx, f, false)
	want := string(policy.ReasonTrustRampNotSatisfied)
	if got.Decision != PolicyDecisionBlocked || got.BlockedReason != want {
		t.Fatalf("after downgrade: %s (%s), want blocked %s",
			got.Decision, got.BlockedReason, want)
	}
	if got.DecisionID <= 0 {
		t.Fatalf("refusal was not recorded in the decision ledger: %+v", got)
	}
	var reason, verdict string
	if err := pool.QueryRow(ctx, `SELECT reason, verdict FROM sage.decision
		WHERE id = $1`, got.DecisionID).Scan(&reason, &verdict); err != nil {
		t.Fatalf("read recorded decision: %v", err)
	}
	if reason != want || verdict != string(policy.VerdictBlocked) {
		t.Fatalf("recorded decision = %s/%s, want blocked/%s", verdict, reason, want)
	}
}

func TestTrustCeilingRaisedRollbackCooldownSkipsFindingOnPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	object := "public.ceiling_cooldown_" + time.Now().UTC().Format("150405000000")
	var findingID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status)
		VALUES ('ceiling_cooldown_probe', 'warning', 'index', $1, 'probe', '{}', 'open')
		RETURNING id`, object).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, finding_id, sql_executed, outcome, executed_at)
		VALUES ('drop_index', $1, 'DROP INDEX CONCURRENTLY x', 'rolled_back',
		        now() - interval '2 days')
		RETURNING id`, findingID).Scan(&actionID); err != nil {
		t.Fatalf("insert rolled-back action: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE id=$1", actionID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id=$1", findingID)
	})
	cfg := windowedAutonomousConfig()
	cfg.Trust.RollbackCooldownDays = 1
	e := New(pool, cfg, time.Time{}, noopExecLog)
	if e.recentlyRolledBack(ctx, findingID) {
		t.Fatal("a 2-day-old rollback is inside a 1-day cooldown")
	}
	raised := ceilingFromTrust(cfg.Trust)
	raised.RollbackCooldownDays = 7
	e.SetTrustCeiling(&raised)
	if !e.recentlyRolledBack(ctx, findingID) {
		t.Fatal("the live 7-day cooldown did not hold the finding rolled back 2 days ago")
	}
	if cfg.Trust.RollbackCooldownDays != 1 {
		t.Fatal("SetTrustCeiling rewrote the shared config")
	}
}
