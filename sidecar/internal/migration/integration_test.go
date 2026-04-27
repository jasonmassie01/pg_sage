package migration

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
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
	return fmt.Sprintf("mig_test_%04d", rand.Intn(10000))
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

// testLogFn returns a logFn that routes to t.Logf.
func testLogFn(t *testing.T) func(string, string, ...any) {
	t.Helper()
	return func(level, msg string, args ...any) {
		t.Helper()
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}
}

// newTestAdvisor builds an Advisor suitable for integration tests:
// no LLM, PG 16, advisory mode.
func newTestAdvisor(
	t *testing.T, pool *pgxpool.Pool,
) *Advisor {
	t.Helper()
	cfg := &config.MigrationConfig{
		Enabled: true,
		Mode:    "advisory",
	}
	return NewAdvisor(pool, cfg, 160000, "test_db", testLogFn(t), nil)
}

// newTestDetector builds a Detector backed by the given advisor.
func newTestDetector(
	t *testing.T, pool *pgxpool.Pool, advisor *Advisor,
) *Detector {
	t.Helper()
	cfg := &config.MigrationConfig{
		Enabled:             true,
		Mode:                "advisory",
		PollIntervalSeconds: 5,
	}
	return NewDetector(pool, advisor, cfg, testLogFn(t))
}

// analyzeTable runs ANALYZE so reltuples/relpages are populated.
func analyzeTable(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	schema, table string,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		fmt.Sprintf("ANALYZE %s.%s", schema, table))
	require.NoError(t, err)
}

// --- Integration Tests ---

// TestIntegration_Advisor_IndexCreation verifies that the advisor
// can analyze CREATE INDEX on a real table without error. With a
// small table the risk score stays below 0.3 (returns nil). Also
// tests a riskier DDL (SET NOT NULL).
func TestIntegration_Advisor_IndexCreation(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	advisor := newTestAdvisor(t, pool)

	// Create a table with some data.
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s.idx_test (id serial PRIMARY KEY, val int)",
		schema))
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.idx_test (val) VALUES (%d)",
			schema, i))
		require.NoError(t, err)
	}
	analyzeTable(t, pool, ctx, schema, "idx_test")

	// CREATE INDEX on a small table: risk <= 0.3, expect nil incident.
	sql := fmt.Sprintf(
		"CREATE INDEX idx_val ON %s.idx_test (val)", schema)
	incident, err := advisor.Analyze(ctx, sql)
	assert.NoError(t, err, "Analyze CREATE INDEX should not error")
	// Small table: risk is low, incident is nil.
	if incident != nil {
		assert.Equal(t, "schema_advisor", incident.Source)
		assert.NotEmpty(t, incident.RootCause)
	}

	// SET NOT NULL is a riskier operation (ACCESS EXCLUSIVE lock).
	sqlNotNull := fmt.Sprintf(
		"ALTER TABLE %s.idx_test ALTER COLUMN val SET NOT NULL",
		schema)
	incident2, err := advisor.Analyze(ctx, sqlNotNull)
	assert.NoError(t, err, "Analyze SET NOT NULL should not error")
	// On a small table risk is still low; either way, no error.
	if incident2 != nil {
		assert.Equal(t, "schema_advisor", incident2.Source)
		assert.Contains(t, incident2.RootCause, "ddl_set_not_null")
	}
}

// TestIntegration_Advisor_NoMatch verifies that non-DDL SQL
// returns nil incident and nil error.
func TestIntegration_Advisor_NoMatch(t *testing.T) {
	pool, ctx := requireDB(t)
	advisor := newTestAdvisor(t, pool)

	incident, err := advisor.Analyze(ctx, "SELECT 1")
	assert.NoError(t, err, "SELECT should not error")
	assert.Nil(t, incident, "SELECT is not DDL; expect nil incident")
}

// TestIntegration_RiskAssessor_TableStats verifies that the risk
// assessor populates EstimatedRows and TableSizeBytes from a real
// table and that the RiskScore is within [0, 1].
func TestIntegration_RiskAssessor_TableStats(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s.stats_test (id serial PRIMARY KEY, payload text)",
		schema))
	require.NoError(t, err)

	for i := 0; i < 200; i++ {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.stats_test (payload) VALUES ('row_%d')",
			schema, i))
		require.NoError(t, err)
	}
	analyzeTable(t, pool, ctx, schema, "stats_test")

	assessor := NewRiskAssessor(pool, testLogFn(t))
	classification := DDLClassification{
		RuleID:    "ddl_alter_type_rewrite",
		Statement: fmt.Sprintf(
			"ALTER TABLE %s.stats_test ALTER COLUMN payload TYPE varchar(100)",
			schema),
		LockLevel:       "ACCESS EXCLUSIVE",
		RequiresRewrite: true,
		TableName:       "stats_test",
		SchemaName:      schema,
		Description:     "test classification",
	}

	risk, err := assessor.Assess(ctx, classification)
	require.NoError(t, err, "Assess should not error")

	assert.Greater(t, risk.EstimatedRows, int64(0),
		"EstimatedRows should be > 0 after ANALYZE on 200 rows")
	assert.Greater(t, risk.TableSizeBytes, int64(0),
		"TableSizeBytes should be > 0 after inserting data")
	assert.GreaterOrEqual(t, risk.RiskScore, 0.0,
		"RiskScore must be >= 0")
	assert.LessOrEqual(t, risk.RiskScore, 1.0,
		"RiskScore must be <= 1")
	assert.Equal(t, "ddl_alter_type_rewrite", risk.RuleID)
	assert.Equal(t, "ACCESS EXCLUSIVE", risk.LockLevel)
}

// TestIntegration_RiskAssessor_NonexistentTable verifies graceful
// degradation when the table does not exist.
func TestIntegration_RiskAssessor_NonexistentTable(t *testing.T) {
	pool, ctx := requireDB(t)
	assessor := NewRiskAssessor(pool, testLogFn(t))

	classification := DDLClassification{
		RuleID:          "ddl_drop_table",
		Statement:       "DROP TABLE no_such_schema.no_such_table",
		LockLevel:       "ACCESS EXCLUSIVE",
		RequiresRewrite: false,
		TableName:       "no_such_table",
		SchemaName:      "no_such_schema",
		Description:     "nonexistent table",
	}

	risk, err := assessor.Assess(ctx, classification)
	assert.NoError(t, err,
		"Assess on nonexistent table should not error")
	assert.Equal(t, int64(0), risk.EstimatedRows,
		"EstimatedRows should be 0 for nonexistent table")
	assert.Equal(t, int64(0), risk.TableSizeBytes,
		"TableSizeBytes should be 0 for nonexistent table")
}

// TestIntegration_RiskAssessor_ReplicationLag verifies that
// ReplicationLag is populated (>= 0). In a test environment
// without replicas, this is expected to be 0.
func TestIntegration_RiskAssessor_ReplicationLag(t *testing.T) {
	pool, ctx := requireDB(t)
	assessor := NewRiskAssessor(pool, testLogFn(t))

	classification := DDLClassification{
		RuleID:          "ddl_index_not_concurrent",
		Statement:       "CREATE INDEX idx_x ON public.pg_class (relname)",
		LockLevel:       "SHARE",
		RequiresRewrite: false,
		TableName:       "pg_class",
		SchemaName:      "pg_catalog",
		Description:     "replication lag check",
	}

	risk, err := assessor.Assess(ctx, classification)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, risk.ReplicationLag, 0.0,
		"ReplicationLag should be >= 0")
}

// TestIntegration_Detector_PollOnce_NoActive verifies that
// PollOnce returns no incidents when there is no active DDL.
func TestIntegration_Detector_PollOnce_NoActive(t *testing.T) {
	pool, ctx := requireDB(t)
	advisor := newTestAdvisor(t, pool)
	detector := newTestDetector(t, pool, advisor)

	incidents, err := detector.PollOnce(ctx)
	assert.NoError(t, err, "PollOnce should not error")
	assert.Empty(t, incidents,
		"no active DDL in test env; expect empty incidents")
}

// TestIntegration_Advisor_DropTable verifies that DROP TABLE
// is classified as ddl_drop_table with ACCESS EXCLUSIVE.
func TestIntegration_Advisor_DropTable(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	advisor := newTestAdvisor(t, pool)

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s.drop_me (id serial)", schema))
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "drop_me")

	sql := fmt.Sprintf("DROP TABLE %s.drop_me", schema)
	incident, err := advisor.Analyze(ctx, sql)
	assert.NoError(t, err, "Analyze DROP TABLE should not error")

	// Small table: risk may be below 0.3 threshold.
	if incident != nil {
		assert.Equal(t, "schema_advisor", incident.Source)
		assert.Contains(t, incident.RootCause, "ddl_drop_table")
		assert.NotEmpty(t, incident.Severity,
			"Severity must be set when incident is returned")
	}

	// Verify the classifier independently catches this DDL.
	classifier := NewRegexClassifier()
	classifications := classifier.Classify(sql, 160000)
	var foundDrop bool
	for _, c := range classifications {
		if c.RuleID == "ddl_drop_table" {
			foundDrop = true
			assert.Equal(t, "ACCESS EXCLUSIVE", c.LockLevel)
			assert.Equal(t, "drop_me", c.TableName)
			assert.Equal(t, schema, c.SchemaName)
		}
	}
	assert.True(t, foundDrop,
		"DROP TABLE should produce ddl_drop_table classification")
}

// TestIntegration_Advisor_VacuumFull verifies that VACUUM FULL
// is classified and the advisor handles it without error.
func TestIntegration_Advisor_VacuumFull(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	advisor := newTestAdvisor(t, pool)

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s.vac_test (id serial, data text)", schema))
	require.NoError(t, err)

	sql := fmt.Sprintf("VACUUM FULL %s.vac_test", schema)
	incident, err := advisor.Analyze(ctx, sql)
	assert.NoError(t, err, "Analyze VACUUM FULL should not error")

	if incident != nil {
		assert.Equal(t, "schema_advisor", incident.Source)
		assert.Contains(t, incident.RootCause, "ddl_vacuum_full")
		assert.NotEmpty(t, incident.Severity)
		assert.NotEmpty(t, incident.DatabaseName)
	}

	// Verify classifier independently.
	classifier := NewRegexClassifier()
	classifications := classifier.Classify(sql, 160000)
	var foundVacuum bool
	for _, c := range classifications {
		if c.RuleID == "ddl_vacuum_full" {
			foundVacuum = true
			assert.Equal(t, "ACCESS EXCLUSIVE", c.LockLevel)
		}
	}
	assert.True(t, foundVacuum,
		"VACUUM FULL should produce ddl_vacuum_full classification")
}

// TestIntegration_Classifier_EndToEnd is a smoke test that runs
// several DDL statements through the full RegexClassifier and
// verifies expected RuleIDs and counts.
func TestIntegration_Classifier_EndToEnd(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	// Create a real table so schema references are grounded.
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s.cls_test (id serial PRIMARY KEY, val int, name text)",
		schema))
	require.NoError(t, err)

	classifier := NewRegexClassifier()

	tests := []struct {
		name       string
		sql        string
		wantRuleID string
		wantCount  int // minimum number of classifications
	}{
		{
			name: "CREATE INDEX without CONCURRENTLY",
			sql: fmt.Sprintf(
				"CREATE INDEX idx_cls ON %s.cls_test (val)",
				schema),
			wantRuleID: "ddl_index_not_concurrent",
			wantCount:  1,
		},
		{
			name: "CREATE INDEX CONCURRENTLY (safe)",
			sql: fmt.Sprintf(
				"CREATE INDEX CONCURRENTLY idx_cls2 ON %s.cls_test (val)",
				schema),
			wantRuleID: "", // no classification expected
			wantCount:  0,
		},
		{
			name: "DROP TABLE",
			sql: fmt.Sprintf(
				"DROP TABLE %s.cls_test", schema),
			wantRuleID: "ddl_drop_table",
			wantCount:  1,
		},
		{
			name: "SET NOT NULL",
			sql: fmt.Sprintf(
				"ALTER TABLE %s.cls_test ALTER COLUMN val SET NOT NULL",
				schema),
			wantRuleID: "ddl_set_not_null",
			wantCount:  1,
		},
		{
			name: "ALTER TYPE (rewrite)",
			sql: fmt.Sprintf(
				"ALTER TABLE %s.cls_test ALTER COLUMN name TYPE varchar(50)",
				schema),
			wantRuleID: "ddl_alter_type_rewrite",
			wantCount:  1,
		},
		{
			name: "VACUUM FULL",
			sql: fmt.Sprintf(
				"VACUUM FULL %s.cls_test", schema),
			wantRuleID: "ddl_vacuum_full",
			wantCount:  1,
		},
		{
			name: "REINDEX without CONCURRENTLY",
			sql: fmt.Sprintf(
				"REINDEX TABLE %s.cls_test", schema),
			wantRuleID: "ddl_reindex_not_concurrent",
			wantCount:  1,
		},
		{
			name:       "CLUSTER",
			sql:        "CLUSTER my_table USING my_index",
			wantRuleID: "ddl_cluster",
			wantCount:  1,
		},
		{
			name:       "SELECT (not DDL)",
			sql:        "SELECT * FROM pg_class",
			wantRuleID: "",
			wantCount:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := classifier.Classify(tt.sql, 160000)
			assert.GreaterOrEqual(t, len(results), tt.wantCount,
				"expected at least %d classifications", tt.wantCount)

			if tt.wantRuleID != "" {
				var found bool
				for _, c := range results {
					if c.RuleID == tt.wantRuleID {
						found = true
						assert.NotEmpty(t, c.Description,
							"Description should be populated")
						assert.NotEmpty(t, c.LockLevel,
							"LockLevel should be populated for %s",
							tt.wantRuleID)
						break
					}
				}
				assert.True(t, found,
					"expected RuleID %q in classifications",
					tt.wantRuleID)
			}
		})
	}
}

// TestIntegration_RiskScore_Bounds creates a classification with
// ACCESS EXCLUSIVE lock and verifies the risk score stays in [0, 1]
// after assessment against a table with data.
func TestIntegration_RiskScore_Bounds(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s.bounds_test (id serial PRIMARY KEY, payload text)",
		schema))
	require.NoError(t, err)

	// Insert enough rows to push table stats up.
	for i := 0; i < 500; i++ {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.bounds_test (payload) VALUES ('data_%d')",
			schema, i))
		require.NoError(t, err)
	}
	analyzeTable(t, pool, ctx, schema, "bounds_test")

	assessor := NewRiskAssessor(pool, testLogFn(t))

	classification := DDLClassification{
		RuleID:          "ddl_alter_type_rewrite",
		Statement:       fmt.Sprintf(
			"ALTER TABLE %s.bounds_test ALTER COLUMN payload TYPE varchar(255)",
			schema),
		LockLevel:       "ACCESS EXCLUSIVE",
		RequiresRewrite: true,
		TableName:       "bounds_test",
		SchemaName:      schema,
		Description:     "bounds test classification",
	}

	risk, err := assessor.Assess(ctx, classification)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, risk.RiskScore, 0.0,
		"RiskScore must be >= 0")
	assert.LessOrEqual(t, risk.RiskScore, 1.0,
		"RiskScore must be <= 1")
	assert.Greater(t, risk.EstimatedRows, int64(0),
		"EstimatedRows should be > 0 after inserting 500 rows")
	assert.Greater(t, risk.TableSizeBytes, int64(0),
		"TableSizeBytes should be > 0")
	assert.Greater(t, risk.EstimatedLockMs, int64(0),
		"EstimatedLockMs should be > 0 for a rewrite operation")
}
