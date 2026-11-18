package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// guard-02c: a plan_flip_after_analyze finding's re-ANALYZE is the
// existing safe analyze_table action. It runs through the executor (gate,
// audit, verification) and records its verdict; nothing else writes.

func TestPlanFlip_ReanalyzeIsTheSafeAnalyzeTableAction(t *testing.T) {
	for _, sql := range []string{"ANALYZE public.orders;", `ANALYZE "Sales"."odd""name";`} {
		action, tier := ClassifySQL(sql)
		if action != "analyze_table" || tier != string(policy.RiskSafe) {
			t.Errorf("ClassifySQL(%q) = %q/%q, want analyze_table/safe", sql, action, tier)
		}
		if got := verificationClass(sql); got != verify.ClassAnalyze {
			t.Errorf("verificationClass(%q) = %q, want analyze", sql, got)
		}
	}
}

// planFlipFixture is a table with rows modified since its last ANALYZE
// and an open plan_flip_after_analyze finding naming it.
func planFlipFixture(t *testing.T) (*pgxpool.Pool, string, int) {
	t.Helper()
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("public.flip_exec_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+
		" AS SELECT g AS id, NULL::int AS v FROM generate_series(1, 5000) g"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	sql := "ANALYZE " + table + ";"
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ('plan_flip_after_analyze', 'warning', 'query', $1, 'plan flip',
		jsonb_build_object('relation', $2::text), 'Run ANALYZE again', $3)
		RETURNING id`, fmt.Sprintf("queryid:%d", time.Now().UnixNano()), table, sql).
		Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, "DELETE FROM sage.action_log WHERE finding_id = $1", id)
		_, _ = pool.Exec(c, "DELETE FROM sage.findings WHERE id = $1", id)
		_, _ = pool.Exec(c, "DROP TABLE IF EXISTS "+table)
	})
	waitModsSinceAnalyze(t, pool, table)
	return pool, table, id
}

// waitModsSinceAnalyze waits until the statistics show the table's rows
// as modified since analyze (PostgreSQL 14 reports through the collector).
func waitModsSinceAnalyze(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var mods int64
		err := pool.QueryRow(context.Background(), `SELECT pg_stat_clear_snapshot(),
			COALESCE(n_mod_since_analyze, 0) FROM pg_stat_user_tables
			WHERE relid = to_regclass($1)`, table).Scan(nil, &mods)
		if err == nil && mods >= 5000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("n_mod_since_analyze of %s = %d (%v), want 5000", table, mods, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestPlanFlip_ExecutorReanalyzesAndRecordsTheVerdict(t *testing.T) {
	pool, table, findingID := planFlipFixture(t)
	exec := withTestStandingGate(manualExecutor(pool))
	ctx := context.Background()
	actionID, err := exec.ExecuteManual(ctx, findingID, "ANALYZE "+table+";", "", nil)
	if err != nil || actionID <= 0 {
		t.Fatalf("ExecuteManual = %d, %v", actionID, err)
	}
	var actionType, outcome string
	var finding int
	if err := pool.QueryRow(ctx, `SELECT action_type, outcome, finding_id
		FROM sage.action_log WHERE id = $1`, actionID).
		Scan(&actionType, &outcome, &finding); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if actionType != "analyze" || outcome != "success" || finding != findingID {
		t.Fatalf("action = %s/%s finding %d, want a successful analyze for %d",
			actionType, outcome, finding, findingID)
	}
	var analyzed *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_analyze FROM pg_stat_user_tables
		WHERE relid = to_regclass($1)`, table).Scan(&analyzed); err != nil || analyzed == nil {
		t.Fatalf("last_analyze = %v (%v), want the re-ANALYZE", analyzed, err)
	}
	var status string
	var linked *int64
	if err := pool.QueryRow(ctx, `SELECT status, action_log_id FROM sage.findings
		WHERE id = $1`, findingID).Scan(&status, &linked); err != nil {
		t.Fatal(err)
	}
	// The analyzer judges the plan from the next capture; the row stays
	// linked to the action it judges.
	if status != "resolved" || linked == nil || *linked != actionID {
		t.Fatalf("finding = %s linked to %v, want resolved by action %d", status, linked,
			actionID)
	}
	got := storedOutcome(t, pool, actionID)
	if got.Class != verify.ClassAnalyze || got.Verdict != verify.OutcomeImproved {
		t.Fatalf("outcome = %+v, want an improved analyze (modifications reset)", got)
	}
}
