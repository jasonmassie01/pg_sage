package cases

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: candidateFromFinding is a pure function.

func noContract(string) (string, string) { return "", "" }

func indexContract(sql string) (string, string) {
	return "create_index_concurrently", "moderate"
}

func TestCandidateFromFindingPicksOnlyChangesPGSageCannotRun(t *testing.T) {
	ddl := prFinding{ID: 7, Title: "Add a check", Recommendation: "why",
		SQL: "ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0) NOT VALID", Rollback: "ALTER x",
		Detail: map[string]any{"ladder": []any{map[string]any{"tier": 1.0,
			"name": "plan_only", "result": "passed", "at": "2026-11-17T09:00:00Z"}}}}
	c, ok := candidateFromFinding(ddl, noContract)
	require.True(t, ok)
	require.Equal(t, PRKindMigration, c.Kind)
	require.Equal(t, "high", c.RiskTier)
	require.Equal(t, ddl.SQL, c.MigrationSQL)
	require.Equal(t, "ALTER x", c.RollbackSQL)
	require.Equal(t, []string{"finding:7"}, c.EvidenceIDs)
	require.Equal(t, 1, len(c.Ladder))
	require.Equal(t, "why", c.Why)

	_, ok = candidateFromFinding(ddl, indexContract)
	require.False(t, ok, "a change the executor runs under a contract is not a PR")
	_, ok = candidateFromFinding(prFinding{ID: 8, Title: "t"}, noContract)
	require.False(t, ok, "no change, no candidate")
	_, ok = candidateFromFinding(prFinding{ID: 9, Title: "t", SQL: "SELECT 1"}, noContract)
	require.False(t, ok, "a read is not a migration")
}

func TestCandidateFromFindingFactBoundSourceFix(t *testing.T) {
	f := prFinding{ID: 3, Title: "Index orders", SQL: "CREATE INDEX CONCURRENTLY i ON o (a)",
		Detail: map[string]any{"source_fix": map[string]any{
			"migration": "CREATE INDEX CONCURRENTLY i ON o (a);", "down": "DROP INDEX i;"}}}
	c, ok := candidateFromFinding(f, indexContract)
	require.True(t, ok, "the application's migrations own the object: a PR, not a run")
	require.Equal(t, "CREATE INDEX CONCURRENTLY i ON o (a);", c.MigrationSQL)
	require.Equal(t, "DROP INDEX i;", c.RollbackSQL)
	require.Equal(t, "moderate", c.RiskTier)
}

func TestCandidateFromFindingRewrite(t *testing.T) {
	f := prFinding{ID: 4, Title: "Rewrite", Detail: map[string]any{
		"queryid": "9223372036854775807", "original_query": "SELECT a FROM t",
		"suggested_rewrite": "SELECT a FROM t LIMIT 10"}}
	c, ok := candidateFromFinding(f, noContract)
	require.True(t, ok)
	require.Equal(t, PRKindRewrite, c.Kind)
	require.Equal(t, int64(9223372036854775807), c.QueryID)
	require.Equal(t, "SELECT a FROM t", c.OriginalSQL)
	require.False(t, c.RewriteChecked, "no equivalence record, not checked")
	f.Detail["rewrite_equivalence"] = "passed"
	c, _ = candidateFromFinding(f, noContract)
	require.True(t, c.RewriteChecked)
	f.Detail["ladder"] = "not an array"
	c, ok = candidateFromFinding(f, noContract)
	require.True(t, ok, "an unreadable ladder is no receipt, not a lost candidate")
	require.Nil(t, c.Ladder)
}
