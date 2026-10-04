package agenttools

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: LintMigration classifies text and reads
// catalog statistics; it keeps no state between calls.

// lintTable creates and analyzes <schema>.t with 5000 rows.
func lintTable(f *fixture) string {
	f.t.Helper()
	t := f.q("t")
	f.exec("CREATE TABLE "+t+" (id bigint PRIMARY KEY, a int, b text)",
		"INSERT INTO "+t+" SELECT i, i % 50, md5(i::text) FROM generate_series(1, 5000) i",
		"ANALYZE "+t)
	return t
}

func maxItemScore(items []LintItem) float64 {
	best := 0.0
	for _, it := range items {
		if it.RiskScore > best {
			best = it.RiskScore
		}
	}
	return best
}

func requireVerdictIn(t *testing.T, res LintResult, allowed ...string) {
	t.Helper()
	for _, v := range allowed {
		if res.Verdict == v {
			return
		}
	}
	t.Fatalf("verdict %q, want one of %v (items %+v)", res.Verdict, allowed, res.Statements)
}

func TestLintMigrationVolatileDefaultRewrites(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	sql := "ALTER TABLE " + table + " ADD COLUMN c text DEFAULT gen_random_uuid()::text"
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: sql})
	require.NoError(t, err)
	require.NotEmpty(t, res.Statements)
	var found bool
	for _, it := range res.Statements {
		if it.RequiresRewrite || it.LockLevel == "ACCESS EXCLUSIVE" {
			found = true
			require.True(t, it.Table == table || it.Table == "t", "item table %q", it.Table)
			require.NotEmpty(t, it.Description)
			require.NotEmpty(t, it.RuleID)
		}
	}
	require.True(t, found, "no rewrite/ACCESS EXCLUSIVE item: %+v", res.Statements)
	requireVerdictIn(t, res, "risky", "review")
	require.Equal(t, maxItemScore(res.Statements), res.MaxRiskScore)
	var hasColumn bool
	f.scalar(&hasColumn, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 't' AND column_name = 'c')`, f.schema)
	require.False(t, hasColumn, "linting executed the migration")
}

func TestLintMigrationPlainCreateIndex(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{
		SQL: "CREATE INDEX i ON " + table + " (a)"})
	require.NoError(t, err)
	var idx *LintItem
	for i := range res.Statements {
		if res.Statements[i].LockLevel == "SHARE" {
			idx = &res.Statements[i]
		}
	}
	require.NotNil(t, idx, "no SHARE-lock item: %+v", res.Statements)
	require.Contains(t, idx.SafeAlternative, "CONCURRENTLY")
	require.False(t, idx.RequiresRewrite)
	require.Positive(t, idx.TableSizeBytes, "size from the analyzed table")
	require.Equal(t, int64(5000), idx.EstimatedRows, "rows from ANALYZE")
	require.Positive(t, idx.RiskScore)
	require.LessOrEqual(t, idx.RiskScore, 1.0)
	require.Equal(t, maxItemScore(res.Statements), res.MaxRiskScore)
	require.False(t, f.relationExists(f.q("i")), "linting created the index")
}

func TestLintMigrationSafeConcurrentIndexWithTimeout(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	sql := "SET lock_timeout = '5s'; CREATE INDEX CONCURRENTLY i2 ON " + table + " (b)"
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: sql})
	require.NoError(t, err)
	requireVerdictIn(t, res, "safe", "review")
	for _, it := range res.Statements {
		require.False(t, it.RequiresRewrite, "CIC flagged as rewrite: %+v", it)
		require.NotEqual(t, "ACCESS EXCLUSIVE", it.LockLevel)
	}
	require.Equal(t, maxItemScore(res.Statements), res.MaxRiskScore)
	require.False(t, f.relationExists(f.q("i2")))
}

func TestLintMigrationNothingRecognized(t *testing.T) {
	f := newFixture(t)
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: "SELECT 1"})
	require.NoError(t, err)
	require.Len(t, res.Statements, 0)
	require.Equal(t, "safe", res.Verdict)
	require.Equal(t, 0.0, res.MaxRiskScore)
	require.True(t, strings.Contains(strings.ToLower(res.Note), "recogni"),
		"note must say nothing was recognized: %q", res.Note)
}

// Linting is text analysis only: a destructive migration is never run.
func TestLintMigrationNeverExecutes(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	sql := "DROP TABLE " + table + "; TRUNCATE " + table
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: sql})
	require.NoError(t, err)
	require.NotEmpty(t, res.Statements, "DROP TABLE is a recognized rule")
	requireVerdictIn(t, res, "risky", "review")
	require.True(t, f.relationExists(table), "lint dropped the table")
	require.Equal(t, int64(5000), f.count(table))
}

func TestLintMigrationMissingLockTimeoutFlagged(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{
		SQL: "ALTER TABLE " + table + " ALTER COLUMN a SET NOT NULL"})
	require.NoError(t, err)
	var rules []string
	for _, it := range res.Statements {
		rules = append(rules, it.RuleID)
	}
	require.Contains(t, rules, "ddl_set_not_null")
	require.Contains(t, rules, "ddl_missing_lock_timeout")
	require.Equal(t, maxItemScore(res.Statements), res.MaxRiskScore)
}

func TestLintMigrationInputBounds(t *testing.T) {
	f := newFixture(t)
	tools := New(f.pool, Options{})
	for _, sql := range []string{"", " ", "\n\t  \n"} {
		_, err := tools.LintMigration(f.ctx, LintRequest{SQL: sql})
		require.ErrorIs(t, err, ErrInvalid, "SQL %q", sql)
	}
	at := "SELECT 1" + strings.Repeat(" ", 100000-len("SELECT 1"))
	require.Equal(t, 100000, len(at))
	_, err := tools.LintMigration(f.ctx, LintRequest{SQL: at})
	require.NoError(t, err, "100000 bytes is the inclusive limit")
	over := at + " "
	_, err = tools.LintMigration(f.ctx, LintRequest{SQL: over})
	require.ErrorIs(t, err, ErrInvalid, "100001 bytes")
}

// Without statistics (never analyzed) the item still scores the rule's
// intrinsic hazard, and the rows are not invented.
func TestLintMigrationUnknownTableStats(t *testing.T) {
	f := newFixture(t)
	fresh := f.q("fresh")
	f.exec("CREATE TABLE " + fresh + " (a int)")
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{
		SQL: "ALTER TABLE " + fresh + " ADD COLUMN c text DEFAULT gen_random_uuid()::text"})
	require.NoError(t, err)
	require.NotEmpty(t, res.Statements)
	require.Positive(t, res.MaxRiskScore)
	for _, it := range res.Statements {
		require.LessOrEqual(t, it.EstimatedRows, int64(0), "rows invented: %+v", it)
	}
	missing, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{
		SQL: "CREATE INDEX j ON " + f.q("no_such_table") + " (a)"})
	require.NoError(t, err, "a table that does not exist yet is linted from text")
	require.NotEmpty(t, missing.Statements)
	require.Equal(t, int64(0), missing.Statements[0].TableSizeBytes)
}
