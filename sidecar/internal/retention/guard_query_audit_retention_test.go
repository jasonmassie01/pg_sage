package retention

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// sage.guard_query_audit (agent_query, spec §6.8) lives in every monitored
// database and ages out after agents.query.audit_retention_days, default
// 30 (§6.17). The cleaner runs per database, so the purge runs where the
// rows are. No concurrency tests: the rule is a value in a list.

func guardQueryAuditRule(t *testing.T, cfg *config.Config) (purgeRule, bool) {
	t.Helper()
	for _, r := range purgeRules(cfg) {
		if r.table == "guard_query_audit" {
			return r, true
		}
	}
	return purgeRule{}, false
}

func TestPurgeRules_GuardQueryAuditUsesItsOwnWindow(t *testing.T) {
	cfg := config.DefaultConfig()
	r, ok := guardQueryAuditRule(t, cfg)
	if !ok {
		t.Fatal("no purge rule for sage.guard_query_audit")
	}
	if r.timeCol != "at" || r.sweepCol != "at" || r.days != 30 || r.extra != "" {
		t.Fatalf("rule = %+v, want at (swept), 30 days, no keep predicate", r)
	}
	cfg.Agents.Query.AuditRetentionDays = 7
	cfg.Retention.ActionsDays = 90
	if r, _ := guardQueryAuditRule(t, cfg); r.days != 7 {
		t.Fatalf("days = %d, want agents.query.audit_retention_days (7)", r.days)
	}
	cfg.Agents.Query.AuditRetentionDays = 0
	if r, _ := guardQueryAuditRule(t, cfg); r.days != 0 {
		t.Fatalf("days = %d, want 0 (kept)", r.days)
	}
}

func TestGuardQueryAuditPurgeSQL_IsBounded(t *testing.T) {
	r, _ := guardQueryAuditRule(t, config.DefaultConfig())
	q := purgeSQL(r, "sage.guard_query_audit", batchSize)
	if !strings.Contains(q, "LIMIT 1000") || !strings.Contains(q, "at < now()") {
		t.Fatalf("purge SQL is not a bounded purge on at:\n%s", q)
	}
}

func TestRun_PurgesExpiredGuardQueryAuditOnly(t *testing.T) {
	_, ctx := requireDB(t)
	pid := "agp_" + strings.Repeat("r", 20)
	tag := uniqueTag("gqa")
	insert := `INSERT INTO sage.guard_query_audit (database_id, principal_id,
		envelope_hash, verdict, task_id, at)
		VALUES ('00000000-0000-4000-8000-0000000000c1', $1, 'h', 'execute', $2,
		now() - $3::interval)`
	for _, age := range []string{"60 days", "31 days", "29 days", "1 hour"} {
		execRetry(t, ctx, insert, pid, tag, age)
	}
	cfg := allDays(365)
	cfg.Agents.Query.AuditRetentionDays = 30
	New(testPool, cfg, noopLog).Run(ctx)
	count := `SELECT count(*) FROM sage.guard_query_audit WHERE task_id = $1`
	if n := countWhere(t, ctx, count, tag); n != 2 {
		t.Fatalf("%d rows remain, want 2 (29 days and 1 hour)", n)
	}
	execRetry(t, ctx, insert, pid, tag, "900 days")
	cfg.Agents.Query.AuditRetentionDays = 0
	New(testPool, cfg, noopLog).Run(ctx)
	if n := countWhere(t, ctx, count, tag); n != 3 {
		t.Fatalf("%d rows remain with retention off, want 3", n)
	}
}

// Perf gate A: with the table large, the purge's generic plan reads it
// through the (at) index, not a sequential scan.
func TestGuardQueryAuditPurge_GenericPlanUsesAnIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "gqa_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.guard_query_audit (database_id,
		principal_id, envelope_hash, verdict, at)
		SELECT '00000000-0000-4000-8000-0000000000c2', 'agp_' || (g % 40), 'h', 'execute',
		now() - (g % 90) * interval '1 day' FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE sage.guard_query_audit"); err != nil {
		t.Fatal(err)
	}
	r, _ := guardQueryAuditRule(t, config.DefaultConfig())
	plans, err := perfgate.ExplainStatements(ctx, pool, []perfgate.Statement{
		{Query: purgeSQL(r, "sage.guard_query_audit", batchSize)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Err != "" {
		t.Fatalf("explain: %+v", plans)
	}
	for _, s := range plans[0].SeqScans {
		if s.Schema == "sage" && s.Relation == "guard_query_audit" {
			t.Fatal("the purge scans sage.guard_query_audit sequentially")
		}
	}
}
