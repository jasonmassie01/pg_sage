package lint

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pg-sage/sidecar/internal/config"
)

// --- helpers local to this file ---

// findingsForSchema filters findings to only those in the given schema.
func findingsForSchema(findings []Finding, schema string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Schema == schema {
			out = append(out, f)
		}
	}
	return out
}

// ruleIDSet builds a set of rule IDs from a slice of findings.
func ruleIDSet(findings []Finding) map[string]bool {
	m := make(map[string]bool, len(findings))
	for _, f := range findings {
		m[f.RuleID] = true
	}
	return m
}

// findingByRule returns the first finding with the given ruleID, or nil.
func findingByRule(findings []Finding, ruleID string) *Finding {
	for i := range findings {
		if findings[i].RuleID == ruleID {
			return &findings[i]
		}
	}
	return nil
}

// testLogFn returns a log function that writes to testing.T.
func testLogFn(t *testing.T) func(string, string, ...any) {
	t.Helper()
	return func(level, msg string, args ...any) {
		t.Helper()
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}
}

// sageSchemaExists returns true if sage.findings is queryable
// (required for lint persistence tests post v0.11 merge).
func sageSchemaExists(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
) bool {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM information_schema.tables
		 WHERE table_schema = 'sage'
		   AND table_name = 'findings'`).Scan(&n)
	return err == nil && n == 1
}

// lintFindingCount returns the number of sage.findings rows that the
// lint subsystem owns for the given rule_id, database_name, and status
// filter. status is "open", "resolved", or "" for all.
func lintFindingCount(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	ruleID, dbName, status string,
) int {
	t.Helper()
	q := `SELECT count(*) FROM sage.findings
	      WHERE category = $1
	        AND detail->>'database_name' = $2`
	args := []any{schemaLintCategoryPrefix + ruleID, dbName}
	if status != "" {
		q += ` AND status = $3`
		args = append(args, status)
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, q, args...).Scan(&n))
	return n
}

// cleanLintFindings removes every lint finding tagged with dbName so a
// persistence test starts from a known-clean state and cleans up
// after itself.
func cleanLintFindings(
	pool *pgxpool.Pool, dbName string,
) {
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM sage.findings
		 WHERE category LIKE $1
		   AND detail->>'database_name' = $2`,
		schemaLintCategoryPrefix+"%", dbName)
}

// createAntiPatternTables creates a set of tables in the given schema
// that each trigger a different lint rule. Returns a cleanup function
// registered via t.Cleanup.
func createAntiPatternTables(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, schema string,
) {
	t.Helper()

	// 1. No primary key (triggers lint_no_primary_key).
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.no_pk (a int, b text)", schema))

	// 2. Wide table with >50 columns (triggers lint_wide_table).
	cols := buildColumns(55)
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.wide_tbl (%s)", schema, cols))

	// 3. Timestamp without timezone (triggers lint_timestamp_no_tz).
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.ts_tbl (id serial, created_at timestamp)",
		schema))

	// 4. char(n) column (triggers lint_char_usage).
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.char_tbl (id serial, code char(10))",
		schema))

	// 5. varchar(255) column (triggers lint_varchar_255).
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.vc255_tbl (id serial, name varchar(255))",
		schema))

	// 6. Serial column on a clean table (triggers lint_serial_usage).
	//    The tables above also use serial, but this one is explicit.
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.serial_tbl (id serial PRIMARY KEY, val text)",
		schema))

	// 7. Nullable unique column (triggers lint_nullable_unique).
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE TABLE %s.null_uniq_tbl (id serial PRIMARY KEY, email text)",
		schema))
	exec(t, pool, ctx, fmt.Sprintf(
		"CREATE UNIQUE INDEX idx_null_uniq ON %s.null_uniq_tbl (email)",
		schema))

	// ANALYZE all tables so reltuples is populated for row-count rules.
	for _, tbl := range []string{
		"no_pk", "wide_tbl", "ts_tbl", "char_tbl",
		"vc255_tbl", "serial_tbl", "null_uniq_tbl",
	} {
		analyzeTable(t, pool, ctx, schema, tbl)
	}
}

// exec is a short helper for DDL that must not fail.
func exec(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string,
) {
	t.Helper()
	_, err := pool.Exec(ctx, sql)
	require.NoError(t, err, "exec: %s", sql)
}

// ---------- Test 1: full scan end-to-end ----------

func TestIntegration_Linter_FullScan(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	createAntiPatternTables(t, pool, ctx, schema)

	cfg := &config.SchemaLintConfig{
		Enabled:      true,
		MinTableRows: -1, // bypass the == 0 default of 1000
	}
	linter := New(pool, cfg, 160000, testLogFn(t))

	findings, err := linter.Scan(ctx)
	require.NoError(t, err)

	scoped := findingsForSchema(findings, schema)
	require.NotEmpty(t, scoped,
		"expected at least one finding in test schema %s", schema)

	ids := ruleIDSet(scoped)

	// Rules that must fire given the anti-pattern tables above.
	// wide_table, char_usage, varchar_255, serial_usage do not
	// filter by MinTableRows, so they always fire.
	// no_primary_key, timestamp_no_tz use MinTableRows but we set -1.
	// nullable_unique has no MinTableRows filter.
	mustFire := []string{
		"lint_no_primary_key",
		"lint_wide_table",
		"lint_timestamp_no_tz",
		"lint_char_usage",
		"lint_varchar_255",
		"lint_serial_usage",
		"lint_nullable_unique",
	}
	for _, rule := range mustFire {
		assert.True(t, ids[rule],
			"expected rule %s to fire for schema %s", rule, schema)
	}

	// Verify finding fields are populated.
	f := findingForTable(scoped, schema, "no_pk")
	require.NotNil(t, f, "expected lint finding for no_pk table")
	assert.Equal(t, "lint_no_primary_key", f.RuleID)
	assert.Equal(t, "warning", f.Severity)
	assert.Equal(t, "schema_design", f.Category)
	assert.NotEmpty(t, f.Description)
	assert.NotEmpty(t, f.Impact)
	assert.NotEmpty(t, f.Suggestion)

	f = findingByRule(scoped, "lint_wide_table")
	require.NotNil(t, f)
	assert.Equal(t, "wide_tbl", f.Table)
	assert.Contains(t, f.Description, "55 columns")

	f = findingByRule(scoped, "lint_char_usage")
	require.NotNil(t, f)
	assert.Equal(t, "char_tbl", f.Table)
	assert.Equal(t, "code", f.Column)
}

// ---------- Test 2: disabled rules ----------

func TestIntegration_Linter_DisabledRules(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	createAntiPatternTables(t, pool, ctx, schema)

	cfg := &config.SchemaLintConfig{
		Enabled:       true,
		MinTableRows:  -1,
		DisabledRules: []string{"lint_wide_table", "lint_serial_usage"},
	}
	linter := New(pool, cfg, 160000, testLogFn(t))

	findings, err := linter.Scan(ctx)
	require.NoError(t, err)

	scoped := findingsForSchema(findings, schema)
	ids := ruleIDSet(scoped)

	// Disabled rules must NOT fire.
	assert.False(t, ids["lint_wide_table"],
		"lint_wide_table should be disabled")
	assert.False(t, ids["lint_serial_usage"],
		"lint_serial_usage should be disabled")

	// Non-disabled rules must still fire.
	assert.True(t, ids["lint_no_primary_key"],
		"lint_no_primary_key should still fire")
	assert.True(t, ids["lint_char_usage"],
		"lint_char_usage should still fire")
	assert.True(t, ids["lint_varchar_255"],
		"lint_varchar_255 should still fire")
}

// ---------- Test 3: exclude schemas ----------

func TestIntegration_Linter_ExcludeSchemas(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	createAntiPatternTables(t, pool, ctx, schema)

	cfg := &config.SchemaLintConfig{
		Enabled:        true,
		MinTableRows:   -1,
		ExcludeSchemas: []string{schema},
	}
	linter := New(pool, cfg, 160000, testLogFn(t))

	findings, err := linter.Scan(ctx)
	require.NoError(t, err)

	scoped := findingsForSchema(findings, schema)
	assert.Empty(t, scoped,
		"excluded schema %s should produce zero findings", schema)
}

// ---------- Test 4: sage persistence (upsert + resolve) ----------
//
// Post v0.11 merge: lint findings land in sage.findings with
// category = 'schema_lint:' + rule_id. The Runner.upsertFindings and
// Runner.resolveCleared methods drive analyzer.UpsertFindings /
// ResolveCleared, so these tests exercise the runner's public-ish
// behavior rather than raw SQL.

func TestIntegration_Runner_SagePersistence(t *testing.T) {
	pool, ctx := requireDB(t)

	if !sageSchemaExists(t, pool, ctx) {
		t.Skip("sage.findings table not present; " +
			"skipping persistence test")
	}
	serializeAcrossPackages(t, ctx, pool)

	dbName := fmt.Sprintf("test_db_%d", time.Now().UnixNano())
	schema := createSchema(t, pool, ctx)
	t.Cleanup(func() { cleanLintFindings(pool, dbName) })

	runner := NewRunner(pool,
		&config.SchemaLintConfig{Enabled: true, MinTableRows: -1},
		160000, dbName, testLogFn(t))

	lf := Finding{
		RuleID:      "lint_test_rule",
		Schema:      schema,
		Table:       "test_tbl",
		Severity:    "warning",
		Category:    "correctness",
		Description: "test finding",
		Impact:      "test impact",
		Suggestion:  "fix it",
		SQL:         "ALTER TABLE ...",
	}

	// --- Phase 1: first upsert creates the row, status=open ---
	require.NoError(t,
		runner.upsertFindings(ctx, []Finding{lf}),
		"first upsert")
	require.Equal(t, 1,
		lintFindingCount(t, pool, ctx, "lint_test_rule", dbName, "open"),
		"finding should exist and be open")

	// --- Phase 2: re-upsert refreshes detail + severity, no duplicate ---
	lf.Description = "test finding updated"
	lf.Suggestion = "fix it v2"
	require.NoError(t,
		runner.upsertFindings(ctx, []Finding{lf}),
		"re-upsert")

	// Still exactly one open row; occurrence_count bumped to 2.
	require.Equal(t, 1,
		lintFindingCount(t, pool, ctx, "lint_test_rule", dbName, "open"),
		"re-upsert should not duplicate")
	var occ int
	var title string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT occurrence_count, title FROM sage.findings
		 WHERE category = $1
		   AND detail->>'database_name' = $2
		   AND status = 'open'`,
		schemaLintCategoryPrefix+"lint_test_rule", dbName,
	).Scan(&occ, &title))
	assert.Equal(t, 2, occ, "occurrence_count should increment on re-upsert")
	assert.Equal(t, "test finding updated", title,
		"title should refresh on re-upsert")

	// --- Phase 3: resolveCleared with empty active set resolves it ---
	require.NoError(t,
		runner.resolveCleared(ctx, []Finding{}),
		"resolve cleared with empty set")

	assert.Equal(t, 0,
		lintFindingCount(t, pool, ctx, "lint_test_rule", dbName, "open"),
		"finding should be resolved (no open rows)")
	assert.Equal(t, 1,
		lintFindingCount(t, pool, ctx, "lint_test_rule", dbName, "resolved"),
		"resolved row should still exist")
}

// ---------- Test 5: Runner constructor ----------

func TestIntegration_Runner_NewRunner(t *testing.T) {
	pool, ctx := requireDB(t)
	_ = ctx

	cfg := &config.SchemaLintConfig{
		Enabled:             true,
		ScanIntervalMinutes: 60,
		MinTableRows:        -1,
	}

	runner := NewRunner(pool, cfg, 160000, "test_db", testLogFn(t))
	require.NotNil(t, runner, "NewRunner should return a non-nil runner")
	require.NotNil(t, runner.linter,
		"Runner.linter should be initialized")
	assert.Equal(t, "test_db", runner.databaseName)
}

// ---------- Test 6: Linter returns empty findings for clean schema ---

func TestIntegration_Linter_CleanSchema(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// Create a table with no anti-patterns.
	exec(t, pool, ctx, fmt.Sprintf(
		`CREATE TABLE %s.clean_tbl (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			name text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now()
		)`, schema))
	analyzeTable(t, pool, ctx, schema, "clean_tbl")

	cfg := &config.SchemaLintConfig{
		Enabled:      true,
		MinTableRows: -1,
	}
	linter := New(pool, cfg, 160000, testLogFn(t))

	findings, err := linter.Scan(ctx)
	require.NoError(t, err)

	scoped := findingsForSchema(findings, schema)
	assert.Empty(t, scoped,
		"a well-designed table should not trigger any lint rules")
}

// ---------- Test 7: upsert uniqueness constraint ----------

func TestIntegration_SagePersistence_UpsertIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)

	if !sageSchemaExists(t, pool, ctx) {
		t.Skip("sage.findings table not present")
	}
	serializeAcrossPackages(t, ctx, pool)

	dbName := fmt.Sprintf("test_db_%d", time.Now().UnixNano())
	schema := createSchema(t, pool, ctx)
	t.Cleanup(func() { cleanLintFindings(pool, dbName) })

	runner := NewRunner(pool,
		&config.SchemaLintConfig{Enabled: true, MinTableRows: -1},
		160000, dbName, testLogFn(t))

	// Upsert twice with the same dedup key — the partial unique index
	// on sage.findings (category, object_identifier) WHERE status='open'
	// enforces uniqueness; occurrence_count increments instead.
	for i := 0; i < 2; i++ {
		lf := Finding{
			RuleID:      "lint_test_idem",
			Schema:      schema,
			Table:       "tbl_idem",
			Column:      "col_a",
			Severity:    "info",
			Category:    "convention",
			Description: fmt.Sprintf("desc iter %d", i),
			Impact:      "impact",
			TableSize:   100,
			QueryCount:  5,
			Suggestion:  "suggestion",
			SQL:         "SELECT 1;",
		}
		require.NoError(t,
			runner.upsertFindings(ctx, []Finding{lf}),
			"upsert iteration %d", i)
	}

	require.Equal(t, 1,
		lintFindingCount(t, pool, ctx, "lint_test_idem", dbName, "open"),
		"upsert should not create duplicate open rows")

	// The title should reflect the second upsert.
	var title string
	var occ int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT title, occurrence_count FROM sage.findings
		 WHERE category = $1
		   AND detail->>'database_name' = $2
		   AND status = 'open'`,
		schemaLintCategoryPrefix+"lint_test_idem", dbName,
	).Scan(&title, &occ))
	assert.Equal(t, "desc iter 1", title,
		"upsert should update title to latest value")
	assert.Equal(t, 2, occ,
		"occurrence_count should reflect two upserts")
}

// ---------- Test 8: resolveCleared with selective active set ----------

func TestIntegration_SagePersistence_ResolveCleared(t *testing.T) {
	pool, ctx := requireDB(t)

	if !sageSchemaExists(t, pool, ctx) {
		t.Skip("sage.findings table not present")
	}
	serializeAcrossPackages(t, ctx, pool)

	dbName := fmt.Sprintf("test_db_%d", time.Now().UnixNano())
	schema := createSchema(t, pool, ctx)
	t.Cleanup(func() { cleanLintFindings(pool, dbName) })

	cfg := &config.SchemaLintConfig{
		Enabled:      true,
		MinTableRows: -1,
	}
	runner := NewRunner(pool, cfg, 160000, dbName, testLogFn(t))

	// Seed two findings via the runner so they land in sage.findings
	// with the correct category/object_identifier/detail shape.
	seed := []Finding{
		{
			RuleID: "lint_persist", Schema: schema, Table: "some_tbl",
			Severity: "info", Category: "convention",
			Description: "desc", Impact: "impact", Suggestion: "sug",
		},
		{
			RuleID: "lint_clear", Schema: schema, Table: "some_tbl",
			Severity: "info", Category: "convention",
			Description: "desc", Impact: "impact", Suggestion: "sug",
		},
	}
	require.NoError(t, runner.upsertFindings(ctx, seed))

	// Now run resolveCleared with only lint_persist in the active set.
	activeFindings := []Finding{seed[0]}
	require.NoError(t, runner.resolveCleared(ctx, activeFindings))

	assert.Equal(t, 1,
		lintFindingCount(t, pool, ctx, "lint_persist", dbName, "open"),
		"lint_persist should remain open")
	assert.Equal(t, 0,
		lintFindingCount(t, pool, ctx, "lint_clear", dbName, "open"),
		"lint_clear should no longer be open")
	assert.Equal(t, 1,
		lintFindingCount(t, pool, ctx, "lint_clear", dbName, "resolved"),
		"lint_clear should be resolved")
}

// ---------- Test 9: resolve all when findings are empty ----------

func TestIntegration_SagePersistence_ResolveAllEmpty(t *testing.T) {
	pool, ctx := requireDB(t)

	if !sageSchemaExists(t, pool, ctx) {
		t.Skip("sage.findings table not present")
	}
	serializeAcrossPackages(t, ctx, pool)

	dbName := fmt.Sprintf("test_db_%d", time.Now().UnixNano())
	schema := createSchema(t, pool, ctx)
	t.Cleanup(func() { cleanLintFindings(pool, dbName) })

	cfg := &config.SchemaLintConfig{
		Enabled:      true,
		MinTableRows: -1,
	}
	runner := NewRunner(pool, cfg, 160000, dbName, testLogFn(t))

	// Seed one lint finding via the runner.
	require.NoError(t, runner.upsertFindings(ctx, []Finding{{
		RuleID: "lint_will_clear", Schema: schema, Table: "tbl",
		Severity: "info", Category: "convention",
		Description: "desc", Impact: "impact", Suggestion: "sug",
	}}))
	require.Equal(t, 1,
		lintFindingCount(t, pool, ctx,
			"lint_will_clear", dbName, "open"),
		"seed should produce one open row")

	// Empty findings slice should resolve all open lint findings for
	// this database_name.
	require.NoError(t,
		runner.resolveCleared(ctx, []Finding{}))

	assert.Equal(t, 0,
		lintFindingCount(t, pool, ctx,
			"lint_will_clear", dbName, "open"),
		"all findings should be resolved when scan returns empty")
}
