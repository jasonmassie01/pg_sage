package agenttools

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// tuning-10: MCP (explain_query) and REST (explain.Explain, which the
// /api/v1/explain handler calls) resolve a query_id through the same
// explain.ResolveQueryID, so they explain the same text.

func TestExplainQueryIDParityMCPAndREST(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	orders := f.ordersTable(1000)
	tools := New(f.pool, Options{})
	rest := explain.New(f.pool, &config.ExplainConfig{Enabled: true, TimeoutMs: 10000},
		func(string, string, ...any) {})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.resetOwnStatements()
		for i := 0; i < 3; i++ {
			f.execArgs("SELECT id FROM "+orders+" WHERE status = $1", "open")
		}
		id, ok := f.statementID(orders + " WHERE status = $1")
		if !ok {
			return []string{"statement missing from pg_stat_statements"}
		}
		text, err := tools.explainText(f.ctx, ExplainRequest{QueryID: QueryID(id)})
		require.NoError(t, err)
		mcp, err := tools.ExplainQuery(f.ctx, ExplainRequest{QueryID: QueryID(id)})
		require.NoError(t, err)
		restRes, err := rest.Explain(f.ctx, explain.ExplainRequest{
			QueryID: explain.QueryID(id), PlanOnly: true})
		require.NoError(t, err)
		require.Equal(t, strings.TrimSpace(text), mcp.Query, "MCP explained other text")
		require.Equal(t, mcp.Query, restRes.Query, "REST explained other text than MCP")
		require.Contains(t, mcp.Query, "status = $1")
		require.Equal(t, restRes.PlanSource, mcp.PlanSource)
		require.NotNil(t, mcp.SlowBecause, "MCP result lacks the rules floor")
		return nil
	})
}

// Both front ends reject an id from no statement of this database; MCP
// keeps its not-found error, REST an invalid request (HTTP 400).
func TestExplainQueryIDParityUnknown(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	tools := New(f.pool, Options{})
	_, err := tools.ExplainQuery(f.ctx, ExplainRequest{QueryID: 4242424242424242})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = explain.New(f.pool, &config.ExplainConfig{}, func(string, string, ...any) {}).
		Explain(f.ctx, explain.ExplainRequest{QueryID: 4242424242424242})
	require.ErrorIs(t, err, explain.ErrExplainInvalidRequest)
	require.ErrorIs(t, err, explain.ErrUnknownQueryID)
}
