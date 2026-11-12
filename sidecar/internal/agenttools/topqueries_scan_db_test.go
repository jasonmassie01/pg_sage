package agenttools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: TopQueries keeps no state between calls.

// selfStatement is the i-th distinct pg_sage-tagged statement: i+1 summed
// terms give each its own queryid.
func selfStatement(i int) string {
	return "SELECT " + strings.Repeat("1 + ", i) + "1 /* pg_sage */"
}

// More pg_sage statements than one bounded scan reads, each heavier than
// the workload: the default rule must still list the workload and count
// every left-out statement (the Ask Sage top_queries regression, where the
// schema bootstrap's own statements filled the scan).
func TestTopQueriesWorkloadIsNotCrowdedOutBySelfStatements(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	orders := f.ordersTable(10)
	workloadSQL := "SELECT count(*) FROM " + orders + " WHERE status = $1"
	selfCount := maxTopScan + 20
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.resetOwnStatements()
		f.execArgs(workloadSQL, "open")
		for i := 0; i < selfCount; i++ {
			f.exec(selfStatement(i), selfStatement(i))
		}
		res, err := New(f.pool, Options{}).TopQueries(f.ctx,
			TopQueriesRequest{Limit: 50, OrderBy: "calls"})
		require.NoError(t, err)
		if findTop(res.Queries, orders, "status") == nil {
			return []string{fmt.Sprintf("workload missing from %d queries (%d excluded)",
				len(res.Queries), res.ExcludedCount)}
		}
		if res.ExcludedCount < selfCount {
			return []string{fmt.Sprintf("excluded %d, want >= %d", res.ExcludedCount,
				selfCount)}
		}
		require.Nil(t, findTop(res.Queries, "pg_sage"), "a pg_sage statement is listed")
		return nil
	})
}

// With nothing but pg_sage's statements the result is empty, not an
// error, and the excluded statements are still counted (the SQL returns
// one row with NULL statement columns).
func TestTopQueriesOnlySelfStatementsListsNothingButCounts(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.resetOwnStatements()
		f.exec(selfStatement(1), selfStatement(2))
		res, err := New(f.pool, Options{}).TopQueries(f.ctx, TopQueriesRequest{})
		require.NoError(t, err)
		require.NotNil(t, res.Queries)
		for _, q := range res.Queries {
			require.False(t, strings.Contains(q.Query, "pg_sage"), "listed: %s", q.Query)
		}
		if res.ExcludedCount < 2 {
			return []string{fmt.Sprintf("excluded %d, want >= 2", res.ExcludedCount)}
		}
		require.Equal(t, "pg_stat_statements", res.Source)
		return nil
	})
}
