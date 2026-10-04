package agenttools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: ExplainQuery holds no state of its own; the
// explain package's own tests cover its cache under concurrency.

func requirePlanJSON(t *testing.T, res ExplainResult) {
	t.Helper()
	require.NotEmpty(t, res.PlanJSON, "no plan returned")
	var plan []map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.PlanJSON), &plan), "plan is not JSON")
	require.Len(t, plan, 1)
	require.NotNil(t, plan[0]["Plan"], "plan JSON has no Plan node")
}

func TestExplainQueryEstimatedPlan(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(2000)
	tools := New(f.pool, Options{})
	res, err := tools.ExplainQuery(f.ctx, ExplainRequest{
		Query: "SELECT * FROM " + orders + " WHERE customer_id = 7"})
	require.NoError(t, err)
	requirePlanJSON(t, res)
	require.Positive(t, res.EstimatedCost)
	require.False(t, res.Analyzed)
	require.Nil(t, res.ActualTimeMs, "an estimate has no actual time")
	require.NotEmpty(t, res.NodeBreakdown)
	require.Contains(t, res.Query, orders)
}

func TestExplainQueryAnalyzeOnTableSelect(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(2000)
	tools := New(f.pool, Options{})
	res, err := tools.ExplainQuery(f.ctx, ExplainRequest{
		Query: "SELECT count(*) FROM " + orders + " WHERE amount < 10", Analyze: true})
	require.NoError(t, err)
	requirePlanJSON(t, res)
	require.True(t, res.Analyzed, "safe read was not analyzed: %s", res.AnalyzeRefused)
	require.Empty(t, res.AnalyzeRefused)
	require.NotNil(t, res.ActualTimeMs)
	require.GreaterOrEqual(t, *res.ActualTimeMs, 0.0)
	require.True(t, strings.Contains(string(res.PlanJSON), "Actual Total Time"),
		"ANALYZE plan carries actual timings")
}

func TestExplainQueryRejectsWritesAndKeepsRows(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(500)
	tools := New(f.pool, Options{})
	for _, sql := range []string{
		"DELETE FROM " + orders,
		"UPDATE " + orders + " SET amount = 0",
		"INSERT INTO " + orders + " SELECT id + 100000, customer_id, status, amount FROM " +
			orders,
		"TRUNCATE " + orders,
		"DROP TABLE " + orders,
	} {
		for _, analyze := range []bool{false, true} {
			_, err := tools.ExplainQuery(f.ctx, ExplainRequest{Query: sql, Analyze: analyze})
			require.ErrorIs(t, err, ErrInvalid, "%s (analyze=%v)", sql, analyze)
		}
	}
	require.Equal(t, int64(500), f.count(orders), "rows changed")
	require.Equal(t, int64(0), f.count(orders+" WHERE amount = 0 AND id % 1000 <> 0"),
		"UPDATE ran")
}

// A data-modifying CTE starts with WITH but is not a read: it is refused,
// or at most planned without ANALYZE. Either way no row changes.
func TestExplainQueryDataModifyingCTENeverRuns(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(300)
	tools := New(f.pool, Options{})
	sql := "WITH d AS (DELETE FROM " + orders + " RETURNING id) SELECT count(*) FROM d"
	res, err := tools.ExplainQuery(f.ctx, ExplainRequest{Query: sql, Analyze: true})
	if err != nil {
		require.ErrorIs(t, err, ErrInvalid)
	} else {
		require.False(t, res.Analyzed, "a DELETE CTE was executed under ANALYZE")
		require.NotEmpty(t, res.AnalyzeRefused)
	}
	require.Equal(t, int64(300), f.count(orders))
}

func TestExplainQueryRejectsMultipleStatements(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(100)
	tools := New(f.pool, Options{})
	for _, sql := range []string{
		"SELECT 1; DROP TABLE " + orders,
		"SELECT 1;DROP TABLE " + orders + ";",
		"SELECT 1 /* x */ ; DELETE FROM " + orders,
		"SELECT $$;$$; DROP TABLE " + orders,
	} {
		_, err := tools.ExplainQuery(f.ctx, ExplainRequest{Query: sql, Analyze: true})
		require.ErrorIs(t, err, ErrInvalid, sql)
	}
	require.True(t, f.relationExists(orders), "table dropped by a smuggled statement")
	require.Equal(t, int64(100), f.count(orders))
}

func TestExplainQueryRequestShape(t *testing.T) {
	f := newFixture(t)
	tools := New(f.pool, Options{})
	_, err := tools.ExplainQuery(f.ctx, ExplainRequest{Query: "SELECT 1", QueryID: 42})
	require.ErrorIs(t, err, ErrInvalid, "both query and queryid")
	_, err = tools.ExplainQuery(f.ctx, ExplainRequest{})
	require.ErrorIs(t, err, ErrInvalid, "neither query nor queryid")
	_, err = tools.ExplainQuery(f.ctx, ExplainRequest{Query: "   \n\t "})
	require.ErrorIs(t, err, ErrInvalid, "blank query")
}

func TestExplainQueryParamsGivePlanWithoutAnalyze(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(1000)
	tools := New(f.pool, Options{})
	res, err := tools.ExplainQuery(f.ctx, ExplainRequest{
		Query: "SELECT * FROM " + orders + " WHERE customer_id = $1", Params: []string{"7"},
		Analyze: true})
	require.NoError(t, err)
	requirePlanJSON(t, res)
	require.False(t, res.Analyzed, "parameterized queries are plan-only")
}

func TestExplainQueryByQueryID(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	orders := f.ordersTable(1000)
	tools := New(f.pool, Options{})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		for i := 0; i < 3; i++ {
			f.execArgs("SELECT id FROM "+orders+" WHERE customer_id = $1", 7)
		}
		id, ok := f.statementID("customer_id = $1")
		if !ok {
			return []string{"statement missing from pg_stat_statements"}
		}
		res, err := tools.ExplainQuery(f.ctx, ExplainRequest{QueryID: QueryID(id),
			Analyze: true})
		require.NoError(t, err)
		requirePlanJSON(t, res)
		require.False(t, res.Analyzed, "a $n statement gets a generic plan, never ANALYZE")
		require.Contains(t, res.Query, "customer_id = $1")
		require.Positive(t, res.EstimatedCost)
		return nil
	})
}

func TestExplainQueryByQueryIDOfWriteIsInvalid(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	orders := f.ordersTable(200)
	tools := New(f.pool, Options{})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.execArgs("UPDATE "+orders+" SET amount = amount WHERE id = $1", 1)
		id, ok := f.statementID("SET amount = amount")
		if !ok {
			return []string{"UPDATE missing from pg_stat_statements"}
		}
		_, err := tools.ExplainQuery(f.ctx, ExplainRequest{QueryID: QueryID(id),
			Analyze: true})
		require.ErrorIs(t, err, ErrInvalid, "a recorded UPDATE must not be explained")
		return nil
	})
	require.Equal(t, int64(200), f.count(orders))
}

func TestExplainQueryUnknownQueryID(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	tools := New(f.pool, Options{})
	_, err := tools.ExplainQuery(f.ctx, ExplainRequest{QueryID: 4242424242424242})
	require.ErrorIs(t, err, ErrNotFound)
	require.False(t, errors.Is(err, ErrInvalid), "unknown id is not-found, not invalid")
}
