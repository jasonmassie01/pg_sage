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

// sage.auth_audit (E1: SSO link, unlink, grant, login and break-glass use)
// ages out on retention.auth_audit_days, default 365, like the other
// time-series. No concurrency tests: the rule is a value in a list.

func authAuditRule(t *testing.T, cfg *config.Config) (purgeRule, bool) {
	t.Helper()
	for _, r := range purgeRules(cfg) {
		if r.table == "auth_audit" {
			return r, true
		}
	}
	return purgeRule{}, false
}

func TestPurgeRules_AuthAuditUsesItsOwnWindow(t *testing.T) {
	cfg := config.DefaultConfig()
	r, ok := authAuditRule(t, cfg)
	if !ok {
		t.Fatal("no purge rule for sage.auth_audit")
	}
	if r.timeCol != "created_at" || r.days != 365 || r.extra != "" {
		t.Fatalf("auth_audit rule = %+v, want created_at, 365 days, no keep predicate", r)
	}
	cfg.Retention.AuthAuditDays = 30
	cfg.Retention.ActionsDays = 7
	if r, _ := authAuditRule(t, cfg); r.days != 30 {
		t.Fatalf("auth_audit days = %d, want retention.auth_audit_days (30)", r.days)
	}
	cfg.Retention.AuthAuditDays = 0
	if r, _ := authAuditRule(t, cfg); r.days != 0 {
		t.Fatalf("auth_audit days = %d, want 0 (disabled)", r.days)
	}
}

func TestRetentionExemptions_NoLongerExemptAuthAudit(t *testing.T) {
	if why, ok := retentionExemptions["auth_audit"]; ok {
		t.Fatalf("auth_audit is still exempt (%q) though it has a purge rule", why)
	}
}

// The purge is one bounded statement (LIMIT batch) that finds expired rows
// through an index on created_at.
func TestAuthAuditPurgeSQL_IsBounded(t *testing.T) {
	r, _ := authAuditRule(t, config.DefaultConfig())
	q := purgeSQL(r, "sage.auth_audit", batchSize)
	if !strings.Contains(q, "LIMIT 1000") || !strings.Contains(q, "created_at < now()") {
		t.Fatalf("purge SQL is not a bounded created_at purge:\n%s", q)
	}
}

func TestRun_PurgesExpiredAuthAuditOnly(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("authaudit")
	insert := `INSERT INTO sage.auth_audit (event, detail, created_at)
		VALUES ($1, '{}'::jsonb, now() - $2::interval)`
	for _, age := range []string{"400 days", "366 days", "364 days", "1 hour"} {
		execRetry(t, ctx, insert, tag, age)
	}
	cfg := allDays(30)
	cfg.Retention.AuthAuditDays = 365
	New(testPool, cfg, noopLog).Run(ctx)
	if n := countWhere(t, ctx,
		`SELECT count(*) FROM sage.auth_audit WHERE event = $1`, tag); n != 2 {
		t.Fatalf("%d auth_audit rows remain, want 2 (364 days and 1 hour)", n)
	}

	execRetry(t, ctx, insert, tag, "900 days")
	New(testPool, allDays(30), noopLog).Run(ctx) // auth_audit_days 0: kept
	if n := countWhere(t, ctx,
		`SELECT count(*) FROM sage.auth_audit WHERE event = $1`, tag); n != 3 {
		t.Fatalf("%d auth_audit rows remain with retention off, want 3", n)
	}
}

// Perf gate A: with sage.auth_audit large, the purge's generic plan reads
// it through an index, not a sequential scan.
func TestAuthAuditPurge_GenericPlanUsesAnIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "auth_audit_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.auth_audit (event, target_user_id,
		created_at) SELECT 'login', g % 50, now() - (g % 500) * interval '1 day'
		FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE sage.auth_audit"); err != nil {
		t.Fatal(err)
	}
	r, _ := authAuditRule(t, config.DefaultConfig())
	plans, err := perfgate.ExplainStatements(ctx, pool, []perfgate.Statement{
		{Query: purgeSQL(r, "sage.auth_audit", batchSize)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Err != "" {
		t.Fatalf("explain: %+v", plans)
	}
	for _, s := range plans[0].SeqScans {
		if s.Schema == "sage" && s.Relation == "auth_audit" {
			t.Fatalf("auth_audit purge scans sage.auth_audit sequentially")
		}
	}
}
