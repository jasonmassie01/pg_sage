package agenttools

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: TopQueries only reads pg_stat_statements and
// sage.explain_cache and keeps no state between calls.

// testExcluded drops pg_sage's own statements and EXPLAINs.
func testExcluded(q string) bool {
	return strings.Contains(q, "pg_sage") ||
		strings.HasPrefix(strings.TrimSpace(strings.ToUpper(q)), "EXPLAIN")
}

const workloadCalls = 6

// topWorkload holds the statements a TopQueries test runs.
type topWorkload struct {
	orders, workload, selfTagged, explained string
}

func newTopWorkload(f *fixture) topWorkload {
	orders := f.ordersTable(3000)
	return topWorkload{
		orders: orders,
		workload: "SELECT count(*) FROM " + orders + " WHERE status = $1 " +
			"/*controller='checkout',route='%2Fcart'*/",
		selfTagged: "/* pg_sage */ SELECT max(amount) FROM " + orders,
		explained:  "EXPLAIN SELECT min(amount) FROM " + orders,
	}
}

// run resets the fixture database's statements and runs the workload.
func (w topWorkload) run(f *fixture) {
	f.resetOwnStatements()
	for i := 0; i < workloadCalls; i++ {
		f.execArgs(w.workload, "open")
	}
	f.exec(w.selfTagged, w.explained)
}

func findTop(qs []TopQuery, fragments ...string) *TopQuery {
	for i := range qs {
		ok := true
		for _, frag := range fragments {
			ok = ok && strings.Contains(qs[i].Query, frag)
		}
		if ok {
			return &qs[i]
		}
	}
	return nil
}

func TestTopQueriesWorkloadTagsAndCustomExclusion(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	w := newTopWorkload(f)
	tools := New(f.pool, Options{Excluded: testExcluded})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		w.run(f)
		if _, ok := f.statementID("max(amount)"); !ok {
			return []string{"pg_sage-tagged statement missing from pg_stat_statements"}
		}
		res, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: "calls"})
		require.NoError(t, err)
		var problems []string
		got := findTop(res.Queries, w.orders, "status")
		if got == nil {
			return append(problems, fmt.Sprintf("workload missing from %d queries",
				len(res.Queries)))
		}
		id, _ := f.statementID("status = $1")
		if int64(got.QueryID) != id || got.Calls < workloadCalls {
			problems = append(problems, fmt.Sprintf("workload = %+v, want queryid %d, "+
				"calls >= %d", *got, id, workloadCalls))
		}
		require.Equal(t, "checkout", got.Tags["controller"], "sqlcommenter tags")
		require.Equal(t, "/cart", got.Tags["route"], "URL-decoded tag value")
		require.Positive(t, got.TotalTimeMs)
		require.InDelta(t, got.TotalTimeMs/float64(got.Calls), got.MeanTimeMs, 1e-6)
		require.Equal(t, int64(got.Calls), got.Rows, "count(*) returns one row per call")
		require.Nil(t, findTop(res.Queries, "max(amount)"), "pg_sage statement listed")
		require.Nil(t, findTop(res.Queries, "min(amount)"), "EXPLAIN statement listed")
		for _, q := range res.Queries {
			require.False(t, testExcluded(q.Query), "excluded statement listed: %s", q.Query)
		}
		require.GreaterOrEqual(t, res.ExcludedCount, 1, "excluded statements are counted")
		require.NotEmpty(t, res.Source)
		return problems
	})
}

func TestTopQueriesDefaultExcludesPgSageStatements(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	w := newTopWorkload(f)
	tools := New(f.pool, Options{})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		w.run(f)
		if _, ok := f.statementID("max(amount)"); !ok {
			return []string{"pg_sage-tagged statement missing from pg_stat_statements"}
		}
		res, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: "calls"})
		require.NoError(t, err)
		if findTop(res.Queries, w.orders, "status") == nil {
			return []string{"workload missing"}
		}
		require.Nil(t, findTop(res.Queries, "pg_sage"), "default exclusion kept pg_sage")
		require.GreaterOrEqual(t, res.ExcludedCount, 1)
		return nil
	})
}

func TestTopQueriesAttachesNewestCachedPlan(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	w := newTopWorkload(f)
	tools := New(f.pool, Options{Excluded: testExcluded})
	plan := `[{"Plan":{"Node Type":"Hash Join","Total Cost":123.5,"Plans":[
		{"Node Type":"Seq Scan","Relation Name":"orders"},
		{"Node Type":"Hash","Plans":[{"Node Type":"Index Scan"}]}]}}]`
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		w.run(f)
		id, ok := f.statementID("status = $1")
		if !ok {
			return []string{"workload missing from pg_stat_statements"}
		}
		f.execArgs("DELETE FROM sage.explain_cache WHERE queryid = $1", id)
		t.Cleanup(func() {
			_, _ = f.pool.Exec(f.ctx, "DELETE FROM sage.explain_cache WHERE queryid=$1", id)
		})
		f.execArgs(`INSERT INTO sage.explain_cache (captured_at, queryid, query_text,
			plan_json, source, total_cost, plan_hash) VALUES
			(now() - interval '1 hour', $1, 'old', '[{"Plan":{"Node Type":"Seq Scan"}}]',
			 'old_source', 999, 'old-hash'),
			(now(), $1, 'new', $2::jsonb, 'auto_explain', 123.5, 'ph-test-1')`, id, plan)
		res, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: "calls",
			IncludePlans: true})
		require.NoError(t, err)
		got := findTop(res.Queries, w.orders, "status")
		if got == nil {
			return []string{"workload missing"}
		}
		require.NotNil(t, got.Plan, "cached plan not attached")
		require.Equal(t, "auto_explain", got.Plan.Source, "newest plan wins")
		require.Equal(t, 123.5, got.Plan.TotalCost)
		require.Equal(t, "ph-test-1", got.Plan.PlanHash)
		require.WithinDuration(t, time.Now(), got.Plan.CapturedAt, time.Minute)
		for _, node := range []string{"Hash Join", "Seq Scan", "Hash", "Index Scan"} {
			require.Contains(t, got.Plan.NodeTypes, node)
		}
		plain, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: "calls"})
		require.NoError(t, err)
		for _, q := range plain.Queries {
			require.Nil(t, q.Plan, "plan attached without IncludePlans: %s", q.Query)
		}
		return nil
	})
}

func TestTopQueriesLimitAndOrderValidation(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	w := newTopWorkload(f)
	w.run(f)
	tools := New(f.pool, Options{Excluded: testExcluded})
	res, err := tools.TopQueries(f.ctx, TopQueriesRequest{})
	require.NoError(t, err, "zero request uses defaults")
	require.LessOrEqual(t, len(res.Queries), 10, "Limit 0 means the default of 10")
	one, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 1})
	require.NoError(t, err)
	require.LessOrEqual(t, len(one.Queries), 1)
	_, err = tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50})
	require.NoError(t, err, "50 is the inclusive maximum")
	for _, bad := range []TopQueriesRequest{
		{Limit: 51}, {Limit: -1}, {OrderBy: "bogus"}, {OrderBy: "TOTAL_TIME"},
		{OrderBy: "total_time; DROP TABLE " + w.orders},
		{OrderBy: "calls DESC, (SELECT 1)"},
	} {
		_, err := tools.TopQueries(f.ctx, bad)
		require.ErrorIs(t, err, ErrInvalid, "request %+v", bad)
	}
	require.True(t, f.relationExists(w.orders), "injected ORDER BY dropped the table")
}

func TestTopQueriesOrderings(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	w := newTopWorkload(f)
	w.run(f)
	f.exec("SELECT pg_sleep(0.05) FROM " + w.orders + " LIMIT 1")
	tools := New(f.pool, Options{Excluded: testExcluded})
	key := map[string]func(TopQuery) float64{
		"":           func(q TopQuery) float64 { return q.TotalTimeMs },
		"total_time": func(q TopQuery) float64 { return q.TotalTimeMs },
		"mean_time":  func(q TopQuery) float64 { return q.MeanTimeMs },
		"calls":      func(q TopQuery) float64 { return float64(q.Calls) },
	}
	for order, value := range key {
		res, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: order})
		require.NoError(t, err, "order %q", order)
		require.NotEmpty(t, res.Queries, "order %q returned nothing", order)
		for i := 1; i < len(res.Queries); i++ {
			require.GreaterOrEqual(t, value(res.Queries[i-1]), value(res.Queries[i]),
				"order %q not descending at %d", order, i)
		}
	}
}

func TestTopQueriesOnlyCurrentDatabase(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	other := extraDatabase(t, f.ctx, "topq_other")
	marker := uniqueName("other_marker_")
	mustExec(t, f, other, "CREATE TABLE "+marker+" (a int)")
	tools := New(f.pool, Options{Excluded: testExcluded})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		for i := 0; i < 3; i++ {
			mustExec(t, f, other, "SELECT count(*) FROM "+marker+", pg_sleep(0.3)")
		}
		var seen bool
		err := other.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_statements
			WHERE strpos(query, $1) > 0 AND strpos(query, 'pg_sleep') > 0)`, marker).Scan(&seen)
		require.NoError(t, err)
		if !seen {
			return []string{"other database's statement missing from pg_stat_statements"}
		}
		res, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: "mean_time"})
		require.NoError(t, err)
		require.Nil(t, findTop(res.Queries, marker), "another database's statement listed")
		return nil
	})
}

func TestTopQueriesWithoutPgStatStatementsIsUnavailable(t *testing.T) {
	f := newFixture(t)
	bare := extraDatabase(t, f.ctx, "topq_nopgss")
	mustExec(t, f, bare, "DROP EXTENSION IF EXISTS pg_stat_statements")
	_, err := New(bare, Options{}).TopQueries(f.ctx, TopQueriesRequest{})
	require.ErrorIs(t, err, ErrUnavailable)
	require.False(t, errors.Is(err, ErrInvalid), "a missing extension is not an invalid request")
}
