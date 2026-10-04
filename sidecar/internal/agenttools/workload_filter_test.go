package agenttools

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
	"github.com/pg-sage/sidecar/internal/workload"
)

// The default exclusion is the one workload rule advice uses (#110):
// pg_sage's own statements and diagnostic tooling are never top queries
// or attribution samples, application statements always are.
func TestDefaultExclusionIsTheWorkloadRule(t *testing.T) {
	tools := New(nil, Options{})
	for query, excluded := range map[string]bool{
		"/* pg_sage */ SELECT count(*) FROM pg_class":   true,
		"EXPLAIN ANALYZE SELECT * FROM orders":          true,
		"  explain (format json) SELECT 1":              true,
		"VACUUM ANALYZE orders":                         true,
		"CREATE INDEX CONCURRENTLY i ON orders (a)":     true,
		"SELECT pg_stat_statements_reset()":             true,
		"COPY orders TO STDOUT":                         true,
		"SELECT * FROM orders WHERE customer_id = $1":   false,
		"UPDATE orders SET status = $1 WHERE id = $2":   false,
		"SELECT explain_text FROM docs WHERE id = $1":   false,
		"WITH x AS (SELECT 1) SELECT * FROM x":          false,
		"/*route='/cart'*/ SELECT * FROM cart_items":    false,
		"INSERT INTO audit (note) VALUES ('vacuum it')": false,
	} {
		require.Equal(t, excluded, tools.opts.Excluded(query), query)
		require.Equal(t, workload.Excluded(query), tools.opts.Excluded(query), query)
	}
}

// A caller's own filter still wins over the default.
func TestCustomExclusionOverridesTheDefault(t *testing.T) {
	tools := New(nil, Options{Excluded: func(string) bool { return false }})
	require.False(t, tools.opts.Excluded("EXPLAIN SELECT 1"))
}
