package agenttools

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: LintMigration keeps no state between calls.

// migration-11: lint applies migration.ddl_row_threshold as the advisor
// does. A metadata-only ACCESS EXCLUSIVE with no lock_timeout on 5000 rows
// is review under the default threshold (10000) and risky under 1000.
func TestLintMigrationAppliesRowThreshold(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	sql := "ALTER TABLE " + table + " DROP COLUMN b"
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: sql})
	require.NoError(t, err)
	require.Equal(t, "review", res.Verdict)
	require.Equal(t, int64(10000), res.RowThreshold)
	res, err = New(f.pool, Options{DDLRowThreshold: 1000}).LintMigration(f.ctx,
		LintRequest{SQL: sql})
	require.NoError(t, err)
	require.Equal(t, "risky", res.Verdict)
	require.Equal(t, int64(1000), res.RowThreshold)
	require.True(t, f.relationExists(table), "lint ran the DDL")
}
