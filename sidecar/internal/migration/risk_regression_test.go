package migration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// fakeLargeTable creates schema.name and makes the planner statistics
// report rows tuples without inserting them (the fixture role is a
// superuser on the disposable test server).
func fakeLargeTable(
	t *testing.T, pool *pgxpool.Pool, schema, name string, rows int64,
) {
	t.Helper()
	ctx := context.Background()
	fqn := schema + "." + name
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s (id int, a int, pw text)", fqn)); err != nil {
		t.Fatalf("create %s: %v", fqn, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE pg_class SET reltuples = $1, relpages = $2
		  WHERE oid = $3::regclass`,
		float64(rows), rows/100, fqn); err != nil {
		t.Fatalf("fake stats for %s: %v", fqn, err)
	}
}

// TestIntegration_Advisor_ReportsEveryRuleOnLargeTable is the G7-B03
// regression: with zero activity, most rules scored <= 0.3 on a
// 5M-row table and were silently dropped.
func TestIntegration_Advisor_ReportsEveryRuleOnLargeTable(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	fakeLargeTable(t, pool, schema, "big", 5_000_000)
	big := schema + ".big"
	cases := map[string]string{
		"ddl_index_not_concurrent":   "CREATE INDEX big_a ON " + big + " (a)",
		"ddl_constraint_not_valid":   "ALTER TABLE " + big + " ADD CONSTRAINT c CHECK (a > 0)",
		"ddl_fk_not_valid":           "ALTER TABLE " + big + " ADD CONSTRAINT f FOREIGN KEY (a) REFERENCES x (id)",
		"ddl_drop_column":            "ALTER TABLE " + big + " DROP COLUMN a",
		"ddl_drop_table":             "DROP TABLE " + big,
		"ddl_attach_partition_no_check": "ALTER TABLE " + big + " ATTACH PARTITION p FOR VALUES IN (1)",
		"ddl_set_not_null":           "ALTER TABLE " + big + " ALTER COLUMN a SET NOT NULL",
		"ddl_alter_type_rewrite":     "ALTER TABLE " + big + " ALTER COLUMN a TYPE bigint",
		"ddl_add_column_volatile_default": "ALTER TABLE " + big + " ADD COLUMN r float DEFAULT random()",
		"ddl_reindex_not_concurrent": "REINDEX TABLE " + big,
		"ddl_vacuum_full":            "VACUUM FULL " + big,
		"ddl_refresh_not_concurrent": "REFRESH MATERIALIZED VIEW " + big,
		"ddl_cluster":                "CLUSTER " + big + " USING big_pkey",
		"ddl_set_tablespace":         "ALTER TABLE " + big + " SET TABLESPACE fast",
	}
	advisor := newTestAdvisor(t, pool)
	for rule, sql := range cases {
		t.Run(rule, func(t *testing.T) {
			inc, err := advisor.Analyze(ctx, sql)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if inc == nil {
				t.Fatalf("no incident for %s on a 5M-row table", rule)
			}
			if inc.SignalIDs[0] != rule {
				t.Fatalf("top rule = %s, want %s", inc.SignalIDs[0], rule)
			}
			if inc.Confidence <= 0.3 || inc.Confidence > 1.0 {
				t.Fatalf("score = %.3f, want (0.3, 1.0]", inc.Confidence)
			}
		})
	}
}

// TestIntegration_Advisor_SmallTableBelowThreshold keeps the
// "small table stays quiet" boundary: ddl_row_threshold is the cut.
func TestIntegration_Advisor_SmallTableBelowThreshold(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	fakeLargeTable(t, pool, schema, "small", 9_999)
	fakeLargeTable(t, pool, schema, "edge", 10_000)
	cfg := &config.MigrationConfig{Enabled: true, Mode: "advisory"}
	advisor := NewAdvisor(pool, cfg, 160000, "db", testLogFn(t), nil)

	inc, err := advisor.Analyze(ctx,
		"ALTER TABLE "+schema+".small ALTER COLUMN a TYPE bigint")
	if err != nil {
		t.Fatalf("Analyze small: %v", err)
	}
	if inc != nil {
		t.Fatalf("idle 9,999-row table produced incident %.3f",
			inc.Confidence)
	}
	inc, err = advisor.Analyze(ctx,
		"ALTER TABLE "+schema+".edge ALTER COLUMN a TYPE bigint")
	if err != nil {
		t.Fatalf("Analyze edge: %v", err)
	}
	if inc == nil {
		t.Fatal("10,000-row table (== default threshold) was suppressed")
	}
}

// TestComputeRiskScore_IntrinsicFloor pins the new formula: the rule's
// intrinsic hazard is the floor and escalation factors only raise it.
func TestComputeRiskScore_IntrinsicFloor(t *testing.T) {
	idx := &DDLRisk{RuleID: "ddl_index_not_concurrent", LockLevel: "SHARE"}
	if got := computeRiskScore(idx); got != 0.3 {
		t.Fatalf("idle CREATE INDEX score = %v, want exactly 0.3", got)
	}
	idx.EstimatedRows = 5_000_000
	if got := computeRiskScore(idx); got <= 0.3 {
		t.Fatalf("5M-row CREATE INDEX score = %v, want > 0.3", got)
	}
	vac := &DDLRisk{RuleID: "ddl_vacuum_full",
		LockLevel: "ACCESS EXCLUSIVE", RequiresRewrite: true}
	if got := computeRiskScore(vac); got != 1.0 {
		t.Fatalf("VACUUM FULL score = %v, want 1.0", got)
	}
	none := &DDLRisk{RuleID: "x", LockLevel: "", ActiveQueries: 500}
	if got := computeRiskScore(none); got != 0 {
		t.Fatalf("no-lock score = %v, want 0", got)
	}
}

// TestMaintenanceRules_ExtractTarget: VACUUM FULL / CLUSTER / REINDEX
// / REFRESH never filled TableName, so size never counted (G7-B03).
func TestMaintenanceRules_ExtractTarget(t *testing.T) {
	cases := []struct{ sql, rule, schema, table string }{
		{"VACUUM FULL s.t", "ddl_vacuum_full", "s", "t"},
		{"VACUUM (FULL, ANALYZE) t", "ddl_vacuum_full", "", "t"},
		{"CLUSTER VERBOSE s.t USING i", "ddl_cluster", "s", "t"},
		{"REINDEX TABLE s.t", "ddl_reindex_not_concurrent", "s", "t"},
		{"REFRESH MATERIALIZED VIEW s.mv",
			"ddl_refresh_not_concurrent", "s", "mv"},
	}
	c := NewRegexClassifier()
	for _, tc := range cases {
		got := findRule(c.Classify(tc.sql, 160000), tc.rule)
		if got == nil {
			t.Fatalf("%q: rule %s missing", tc.sql, tc.rule)
		}
		if got.TableName != tc.table || got.SchemaName != tc.schema {
			t.Fatalf("%q: target = %s.%s, want %s.%s", tc.sql,
				got.SchemaName, got.TableName, tc.schema, tc.table)
		}
	}
}

// TestIntegration_VolatileAllowlistMatchesCatalog cross-checks the
// allowlist against pg_proc so it can never drift again (G7-B04).
func TestIntegration_VolatileAllowlistMatchesCatalog(t *testing.T) {
	pool, ctx := requireDB(t)
	for fn := range immutableDefaults {
		var vol string
		err := pool.QueryRow(ctx,
			`SELECT provolatile::text FROM pg_proc
			  WHERE proname = $1 ORDER BY provolatile LIMIT 1`, fn,
		).Scan(&vol)
		if err != nil {
			continue // SQL-standard keywords (current_date) are not in pg_proc
		}
		if vol == "v" {
			t.Errorf("allowlisted default %s is VOLATILE in pg_proc", fn)
		}
	}
}

// TestAdvisor_LockTimeoutIsAnnotation is the G7-B33 regression: the
// rule had weight 0 and could never surface.
func TestIntegration_Advisor_LockTimeoutIsAnnotation(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	fakeLargeTable(t, pool, schema, "lt", 5_000_000)
	inc, err := newTestAdvisor(t, pool).Analyze(ctx,
		"ALTER TABLE "+schema+".lt ALTER COLUMN a SET NOT NULL")
	if err != nil || inc == nil {
		t.Fatalf("Analyze: inc=%v err=%v", inc, err)
	}
	found := false
	for _, link := range inc.CausalChain {
		if link.Signal == "ddl_missing_lock_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing lock_timeout annotation in chain %+v",
			inc.CausalChain)
	}
}

// TestIndexSafeAlternative_IsFormatted is the G7-B34 regression: the
// literal "%s" template leaked into recommended_sql.
func TestIndexSafeAlternative_IsFormatted(t *testing.T) {
	c := NewRegexClassifier()
	got := findRule(c.Classify(
		"CREATE INDEX IF NOT EXISTS i ON s.t (a)", 160000),
		"ddl_index_not_concurrent")
	if got == nil {
		t.Fatal("rule missing")
	}
	want := "CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON s.t (a)"
	if got.SafeAlternative != want {
		t.Fatalf("SafeAlternative = %q, want %q", got.SafeAlternative, want)
	}
	if strings.Contains(got.SafeAlternative, "%s") {
		t.Fatal("placeholder leaked")
	}
}

// TestRuleText_VolatileDefault is the first half of G7-B35.
func TestRuleText_VolatileDefault(t *testing.T) {
	for _, r := range ruleCatalog() {
		if r.ID == "ddl_add_column_volatile_default" &&
			strings.Contains(r.Description, "PG < 11") {
			t.Fatalf("description still claims PG<11 only: %q",
				r.Description)
		}
	}
}

// TestIntegration_AlterType_BinaryCoercible is the second half of
// G7-B35: varchar(n)->varchar(m>=n) and varchar->text do not rewrite.
func TestIntegration_AlterType_BinaryCoercible(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	fqn := schema + ".bc"
	if _, err := pool.Exec(ctx, "CREATE TABLE "+fqn+
		" (v varchar(20), w varchar(20), n int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE pg_class SET reltuples = 5e6
		WHERE oid = $1::regclass`, fqn); err != nil {
		t.Fatalf("fake stats: %v", err)
	}
	advisor := newTestAdvisor(t, pool)
	for _, sql := range []string{
		"ALTER TABLE " + fqn + " ALTER COLUMN v TYPE varchar(40)",
		"ALTER TABLE " + fqn + " ALTER COLUMN w TYPE text",
	} {
		inc, err := advisor.Analyze(ctx, sql)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if inc != nil && inc.SignalIDs[0] == "ddl_alter_type_rewrite" &&
			inc.Severity == "critical" {
			t.Fatalf("%q flagged as critical rewrite", sql)
		}
	}
	inc, err := advisor.Analyze(ctx,
		"ALTER TABLE "+fqn+" ALTER COLUMN n TYPE bigint")
	if err != nil || inc == nil || inc.Severity != "critical" {
		t.Fatalf("int->bigint rewrite not critical: inc=%+v err=%v", inc, err)
	}
}
