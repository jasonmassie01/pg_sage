package migration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

func mustExecAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("fixture %q: %v", sql, err)
		}
	}
}

// Invalid input: an unqualified table whose DDL role has search_path=app
// gets the app table's real stats, not public's (or none).
func TestAssess_UnqualifiedTableUsesRoleSearchPath(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS nr_app CASCADE;
			DROP ROLE IF EXISTS nr_app_role`)
	})
	mustExecAll(t, ctx, pool,
		"DROP SCHEMA IF EXISTS nr_app CASCADE",
		"DROP ROLE IF EXISTS nr_app_role",
		"CREATE ROLE nr_app_role",
		"CREATE SCHEMA nr_app",
		"CREATE TABLE nr_app.nr_orders (id int)",
		"INSERT INTO nr_app.nr_orders SELECT generate_series(1, 1000)",
		"ANALYZE nr_app.nr_orders",
		"ALTER ROLE nr_app_role SET search_path = nr_app")
	ra := NewRiskAssessor(pool, func(string, string, ...any) {})
	risk, err := ra.Assess(ctx, DDLClassification{
		Statement: "ALTER TABLE nr_orders ADD COLUMN x int", LockLevel: "ACCESS EXCLUSIVE",
		TableName: "nr_orders", SessionRole: "nr_app_role"})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if risk.SchemaName != "nr_app" || risk.EstimatedRows != 1000 || !risk.StatsKnown ||
		risk.NameAmbiguous {
		t.Fatalf("risk = schema %q rows %d known %v ambiguous %v; want nr_app's 1,000 rows",
			risk.SchemaName, risk.EstimatedRows, risk.StatsKnown, risk.NameAmbiguous)
	}
}

// Two same-named tables outside the search path are ambiguous: stats are
// unknown rather than taken from the wrong table.
func TestAssess_SameNamedTablesAreAmbiguous(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP SCHEMA IF EXISTS nr_s1 CASCADE; DROP SCHEMA IF EXISTS nr_s2 CASCADE")
	})
	mustExecAll(t, ctx, pool,
		"DROP SCHEMA IF EXISTS nr_s1 CASCADE", "DROP SCHEMA IF EXISTS nr_s2 CASCADE",
		"CREATE SCHEMA nr_s1", "CREATE SCHEMA nr_s2",
		"CREATE TABLE nr_s1.nr_dup (id int)", "CREATE TABLE nr_s2.nr_dup (id int)",
		"INSERT INTO nr_s1.nr_dup SELECT generate_series(1, 500)", "ANALYZE nr_s1.nr_dup",
		"CREATE TABLE nr_s1.nr_only (id int)",
		"INSERT INTO nr_s1.nr_only SELECT generate_series(1, 300)", "ANALYZE nr_s1.nr_only")
	ra := NewRiskAssessor(pool, func(string, string, ...any) {})
	risk, err := ra.Assess(ctx, DDLClassification{
		Statement: "ALTER TABLE nr_dup ADD COLUMN x int", LockLevel: "ACCESS EXCLUSIVE",
		TableName: "nr_dup"})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if !risk.NameAmbiguous || risk.StatsKnown || risk.EstimatedRows != 0 ||
		risk.SchemaName != "" {
		t.Fatalf("risk = schema %q rows %d known %v ambiguous %v; want ambiguous, unknown",
			risk.SchemaName, risk.EstimatedRows, risk.StatsKnown, risk.NameAmbiguous)
	}
	only, err := ra.Assess(ctx, DDLClassification{
		Statement: "ALTER TABLE nr_only ADD COLUMN x int", LockLevel: "ACCESS EXCLUSIVE",
		TableName: "nr_only"})
	if err != nil || only.SchemaName != "nr_s1" || only.EstimatedRows != 300 {
		t.Fatalf("unique relname: %+v (%v), want nr_s1's 300 rows", only, err)
	}
}

// Error propagation: a slow refineAlterType read is cut off by
// catalogread, logged at debug, and the analysis continues (the change
// is conservatively kept as a rewrite).
func TestRefineAlterType_SlowReadCutOff(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.nr_alter")
	})
	mustExecAll(t, ctx, pool, "DROP TABLE IF EXISTS public.nr_alter",
		"CREATE TABLE public.nr_alter (c varchar(10))")
	logs := &riskLog{}
	cfg := &config.MigrationConfig{Enabled: true}
	a := NewAdvisor(pool, cfg, 170000, "db", logs.log, nil).
		WithCatalogReadTimeouts(catalogread.Timeouts{Statement: 200 * time.Millisecond})
	slow := catalogread.WithBeforeStatement(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
		return err
	})
	c := DDLClassification{RuleID: "ddl_alter_type_rewrite", RequiresRewrite: true,
		TableName: "nr_alter", SchemaName: "public", ColumnName: "c", TargetType: "text"}
	start := time.Now()
	a.refineAlterType(slow, &c)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("refineAlterType under a slow read took %s", el)
	}
	if !c.RequiresRewrite {
		t.Fatal("an unread column type was treated as binary coercible")
	}
	found := false
	for _, l := range logs.lines {
		if strings.HasPrefix(l, "debug ") && strings.Contains(l, "column type lookup") &&
			strings.Contains(l, "statement timeout") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no debug log of the cut-off read: %v", logs.lines)
	}
	a.refineAlterType(ctx, &c) // the next read is not slowed: varchar -> text
	if c.RequiresRewrite {
		t.Fatal("varchar -> text still a rewrite once the read succeeds")
	}
}
