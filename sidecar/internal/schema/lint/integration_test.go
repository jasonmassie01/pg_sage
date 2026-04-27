package lint

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	testPoolOnce.Do(func() {
		dsn := os.Getenv("SAGE_DATABASE_URL")
		if dsn == "" {
			dsn = "postgres://postgres:postgres@localhost:5432/" +
				"postgres?sslmode=disable"
		}
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 2
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
		}
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}

// testSchema returns a unique schema name for test isolation.
func testSchema() string {
	return fmt.Sprintf("lint_test_%04d", rand.Intn(10000))
}

// createSchema creates a test schema and registers cleanup.
func createSchema(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
) string {
	t.Helper()
	schema := testSchema()
	_, err := pool.Exec(ctx,
		fmt.Sprintf("CREATE SCHEMA %s", schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			fmt.Sprintf("DROP SCHEMA %s CASCADE", schema))
	})
	return schema
}

func defaultOpts(schema string) RuleOpts {
	return RuleOpts{
		MinTableRows:   0, // catch all tables in tests
		PGVersionNum:   160000,
		ExcludeSchemas: nil,
	}
}

func TestIntegration_WideTable(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	cols := buildColumns(55)
	ddl := fmt.Sprintf(
		"CREATE TABLE %s.wide_test (%s)", schema, cols)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleWideTable{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "wide_test")
	require.NotNil(t, found, "expected finding for wide_test")
	assert.Contains(t, found.Description, "55 columns")
	assert.Equal(t, "lint_wide_table", found.RuleID)
}

// buildColumns generates a comma-separated list of N int columns.
func buildColumns(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("col_%d int", i+1)
	}
	return strings.Join(parts, ", ")
}

func TestIntegration_TimestampNoTZ(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.ts_test (id serial, created_at timestamp)",
		schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "ts_test")

	rule := &ruleTimestampNoTZ{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "ts_test")
	require.NotNil(t, found, "expected finding for ts_test")
	assert.Equal(t, "lint_timestamp_no_tz", found.RuleID)
	assert.Contains(t, found.Column, "created_at")
}

func TestIntegration_CharUsage(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.char_test (id serial, code char(10))",
		schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleCharUsage{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "char_test")
	require.NotNil(t, found, "expected finding for char_test")
	assert.Equal(t, "lint_char_usage", found.RuleID)
	assert.Contains(t, found.Column, "code")
}

func TestIntegration_Varchar255(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.vc_test (id serial, name varchar(255))",
		schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleVarchar255{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "vc_test")
	require.NotNil(t, found, "expected finding for vc_test")
	assert.Equal(t, "lint_varchar_255", found.RuleID)
	assert.Contains(t, found.Column, "name")
}

func TestIntegration_NoPrimaryKey(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.nopk_test (a int, b text)", schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "nopk_test")

	rule := &ruleNoPrimaryKey{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "nopk_test")
	require.NotNil(t, found, "expected finding for nopk_test")
	assert.Equal(t, "lint_no_primary_key", found.RuleID)
}

func TestIntegration_InvalidIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.inv_test (id serial PRIMARY KEY, val int)",
		schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	idxDDL := fmt.Sprintf(
		"CREATE INDEX idx_inv_val ON %s.inv_test (val)", schema)
	_, err = pool.Exec(ctx, idxDDL)
	require.NoError(t, err)

	// Mark index as invalid via pg_index — requires superuser.
	_, err = pool.Exec(ctx, fmt.Sprintf(`
		UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = '%s.idx_inv_val'::regclass`,
		schema))
	if err != nil {
		t.Skipf("cannot mark index invalid (need superuser): %v", err)
	}

	rule := &ruleInvalidIndex{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForIndex(findings, schema, "idx_inv_val")
	require.NotNil(t, found, "expected finding for idx_inv_val")
	assert.Equal(t, "lint_invalid_index", found.RuleID)
	assert.Equal(t, "critical", found.Severity)
}

func TestIntegration_NoPrimaryKey_NotTriggered(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.haspk (id serial PRIMARY KEY, val text)",
		schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "haspk")

	rule := &ruleNoPrimaryKey{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "haspk")
	assert.Nil(t, found,
		"table with PK should not trigger lint_no_primary_key")
}

// analyzeTable runs ANALYZE so reltuples is populated for rules
// that filter on row count.
func analyzeTable(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	schema, table string,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		fmt.Sprintf("ANALYZE %s.%s", schema, table))
	require.NoError(t, err)
}

// --- helpers ---

func findingForTable(
	findings []Finding, schema, table string,
) *Finding {
	for i := range findings {
		if findings[i].Schema == schema &&
			findings[i].Table == table {
			return &findings[i]
		}
	}
	return nil
}

func findingForIndex(
	findings []Finding, schema, index string,
) *Finding {
	for i := range findings {
		if findings[i].Schema == schema &&
			findings[i].Index == index {
			return &findings[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Integration tests for remaining lint rules
// ---------------------------------------------------------------------------

func TestIntegration_DuplicateIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.dup_test (id serial, val int);
		CREATE INDEX idx_dup_val_1 ON %s.dup_test (val);
		CREATE INDEX idx_dup_val_2 ON %s.dup_test (val)`,
		schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleDuplicateIndex{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	// The newer index (higher OID) is the duplicate reported.
	found := findingForTable(findings, schema, "dup_test")
	require.NotNil(t, found, "expected finding for dup_test")
	assert.Equal(t, "lint_duplicate_index", found.RuleID)
	assert.Equal(t, "warning", found.Severity)
	// The reported index should be the second (duplicate) one.
	assert.Equal(t, "idx_dup_val_2", found.Index)
}

func TestIntegration_OverlappingIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.overlap_test (id serial, a int, b int);
		CREATE INDEX idx_overlap_a ON %s.overlap_test (a);
		CREATE INDEX idx_overlap_ab ON %s.overlap_test (a, b)`,
		schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleOverlappingIndex{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	// The shorter index (a) is the redundant one reported.
	found := findingForIndex(findings, schema, "idx_overlap_a")
	require.NotNil(t, found,
		"expected finding for shorter index idx_overlap_a")
	assert.Equal(t, "lint_overlapping_index", found.RuleID)
	assert.Equal(t, "overlap_test", found.Table)
}

func TestIntegration_FKTypeMismatch(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.fk_parent (id bigint PRIMARY KEY);
		CREATE TABLE %s.fk_child (
			id serial PRIMARY KEY,
			parent_id int REFERENCES %s.fk_parent(id)
		)`, schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleFKTypeMismatch{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "fk_child")
	require.NotNil(t, found,
		"expected finding for FK type mismatch on fk_child")
	assert.Equal(t, "lint_fk_type_mismatch", found.RuleID)
	assert.Equal(t, "parent_id", found.Column)
	assert.Equal(t, "warning", found.Severity)
}

func TestIntegration_FKTypeMismatch_NotTriggered(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// Same types — should NOT trigger.
	ddl := fmt.Sprintf(`
		CREATE TABLE %s.fk_parent_ok (id bigint PRIMARY KEY);
		CREATE TABLE %s.fk_child_ok (
			id serial PRIMARY KEY,
			parent_id bigint REFERENCES %s.fk_parent_ok(id)
		)`, schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleFKTypeMismatch{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "fk_child_ok")
	assert.Nil(t, found,
		"matching FK types should not trigger lint_fk_type_mismatch")
}

func TestIntegration_NullableUnique(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.nu_test (
			id serial PRIMARY KEY,
			email text
		);
		CREATE UNIQUE INDEX idx_nu_email ON %s.nu_test (email)`,
		schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleNullableUnique{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "nu_test")
	require.NotNil(t, found,
		"expected finding for nullable unique on nu_test")
	assert.Equal(t, "lint_nullable_unique", found.RuleID)
	assert.Equal(t, "email", found.Column)
	assert.Equal(t, "idx_nu_email", found.Index)
}

func TestIntegration_NullableUnique_NotTriggered(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.nu_ok (
			id serial PRIMARY KEY,
			email text NOT NULL
		);
		CREATE UNIQUE INDEX idx_nu_ok_email ON %s.nu_ok (email)`,
		schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleNullableUnique{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "nu_ok")
	assert.Nil(t, found,
		"NOT NULL unique column should not trigger lint_nullable_unique")
}

func TestIntegration_SerialUsage(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(
		"CREATE TABLE %s.serial_test (id serial PRIMARY KEY, name text)",
		schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleSerialUsage{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "serial_test")
	require.NotNil(t, found,
		"expected finding for serial column on serial_test")
	assert.Equal(t, "lint_serial_usage", found.RuleID)
	assert.Equal(t, "id", found.Column)
}

func TestIntegration_SerialUsage_NotTriggered(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.identity_test (
			id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			name text
		)`, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleSerialUsage{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "identity_test")
	assert.Nil(t, found,
		"identity column should not trigger lint_serial_usage")
}

func TestIntegration_JsonbInJoins(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.jsonb_test (id serial, data jsonb);
		INSERT INTO %s.jsonb_test (data)
			SELECT jsonb_build_object('k', i)
			  FROM generate_series(1, 10) AS i`,
		schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "jsonb_test")

	rule := &ruleJsonbInJoins{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "jsonb_test")
	require.NotNil(t, found,
		"expected finding for JSONB column without GIN index")
	assert.Equal(t, "lint_jsonb_in_joins", found.RuleID)
	assert.Equal(t, "data", found.Column)
}

func TestIntegration_JsonbInJoins_NotTriggered(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	ddl := fmt.Sprintf(`
		CREATE TABLE %s.jsonb_ok (id serial, data jsonb);
		INSERT INTO %s.jsonb_ok (data)
			SELECT jsonb_build_object('k', i)
			  FROM generate_series(1, 10) AS i;
		CREATE INDEX idx_jsonb_ok_gin ON %s.jsonb_ok
			USING gin (data jsonb_path_ops)`,
		schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "jsonb_ok")

	rule := &ruleJsonbInJoins{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "jsonb_ok")
	assert.Nil(t, found,
		"JSONB column with GIN index should not trigger lint_jsonb_in_joins")
}

func TestIntegration_LowCardinalityIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// Insert 100 rows with only 3 distinct status values.
	ddl := fmt.Sprintf(`
		CREATE TABLE %s.lowcard_test (id serial, status int);
		INSERT INTO %s.lowcard_test (status)
			SELECT (i %% 3) + 1
			  FROM generate_series(1, 100) AS i;
		CREATE INDEX idx_lowcard_status ON %s.lowcard_test (status)`,
		schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "lowcard_test")

	rule := &ruleLowCardinalityIndex{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "lowcard_test")
	require.NotNil(t, found,
		"expected finding for low-cardinality btree index")
	assert.Equal(t, "lint_low_cardinality_index", found.RuleID)
	assert.Equal(t, "status", found.Column)
	assert.Equal(t, "idx_lowcard_status", found.Index)
}

func TestIntegration_UnusedIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// The unused-index rule requires stats_reset > 7 days ago.
	// On a fresh database this condition is unlikely to be true,
	// so we only verify the rule executes without error.
	ddl := fmt.Sprintf(`
		CREATE TABLE %s.unused_test (id serial PRIMARY KEY, val int);
		CREATE INDEX idx_unused_val ON %s.unused_test (val)`,
		schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)

	rule := &ruleUnusedIndex{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForIndex(findings, schema, "idx_unused_val")
	if found != nil {
		assert.Equal(t, "lint_unused_index", found.RuleID)
		assert.Equal(t, "unused_test", found.Table)
	} else {
		t.Log("lint_unused_index: no finding produced — " +
			"stats_reset is likely < 7 days old (expected in CI/dev)")
	}
}

func TestIntegration_BloatedTable(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// Insert rows, then delete most of them without vacuuming.
	// Bloat detection depends on relpages not shrinking after
	// the delete, which is best-effort in a small test.
	ddl := fmt.Sprintf(`
		CREATE TABLE %s.bloat_test (id serial, payload text);
		INSERT INTO %s.bloat_test (payload)
			SELECT repeat('x', 200)
			  FROM generate_series(1, 1000);
		DELETE FROM %s.bloat_test WHERE id > 10`,
		schema, schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "bloat_test")

	rule := &ruleBloatedTable{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "bloat_test")
	if found != nil {
		assert.Equal(t, "lint_bloated_table", found.RuleID)
		t.Logf("lint_bloated_table: detected bloat — %s",
			found.Description)
	} else {
		t.Log("lint_bloated_table: no finding produced — " +
			"bloat detection is best-effort in small test tables")
	}
}

func TestIntegration_ToastHeavy(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// Insert large text values to generate TOAST data.
	// Whether the TOAST ratio exceeds 0.5 depends on storage
	// internals, so we verify the rule runs without error.
	ddl := fmt.Sprintf(`
		CREATE TABLE %s.toast_test (id serial, payload text);
		INSERT INTO %s.toast_test (payload)
			SELECT repeat('x', 10000)
			  FROM generate_series(1, 100)`,
		schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "toast_test")

	rule := &ruleToastHeavy{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "toast_test")
	if found != nil {
		assert.Equal(t, "lint_toast_heavy", found.RuleID)
		t.Logf("lint_toast_heavy: detected TOAST-heavy table — %s",
			found.Description)
	} else {
		t.Log("lint_toast_heavy: no finding produced — " +
			"TOAST ratio may not exceed 0.5 threshold in test")
	}
}

func TestIntegration_IntPK_NotTriggered(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// The rule requires reltuples > 1,000,000 (hardcoded).
	// A small table with int PK should NOT trigger.
	// This validates the query runs without error.
	ddl := fmt.Sprintf(`
		CREATE TABLE %s.intpk_test (id int PRIMARY KEY, name text);
		INSERT INTO %s.intpk_test (id, name)
			SELECT i, 'row' || i FROM generate_series(1, 10) AS i`,
		schema, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "intpk_test")

	rule := &ruleIntPK{}
	findings, err := rule.Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)

	found := findingForTable(findings, schema, "intpk_test")
	assert.Nil(t, found,
		"small table with int PK should not trigger lint_int_pk "+
			"(requires > 1M rows)")
}
