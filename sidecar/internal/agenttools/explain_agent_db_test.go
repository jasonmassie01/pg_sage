package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// security-02: explain.enabled gates explain_query for everyone, read on
// every call; agents get plan-only EXPLAIN from a planner that runs as
// their own role (the agent broker), never on pg_sage's pool.

// recordingPlanner is an agent planner that records what it was asked.
type recordingPlanner struct {
	mu     sync.Mutex
	calls  int
	req    readapi.ExplainRequest
	result readapi.ExplainResult
	err    error
}

func (p *recordingPlanner) plan(_ context.Context, req readapi.ExplainRequest) (
	readapi.ExplainResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.req = req
	return p.result, p.err
}

const samplePlan = `[{"Plan":{"Node Type":"Index Scan","Relation Name":"orders",` +
	`"Total Cost":8.29,"Plan Rows":1}}]`

func plannedOK() *recordingPlanner {
	return &recordingPlanner{result: readapi.ExplainResult{Verdict: readapi.VerdictExecute,
		Status: readapi.StatusOK, PlanJSON: json.RawMessage(samplePlan),
		EnvelopeHash: strings.Repeat("a", 64)}}
}

func TestExplainDisabledIsUnavailableAndReadPerCall(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(200)
	cfg := config.ExplainConfig{Enabled: false}
	tools := New(f.pool, Options{Explain: &cfg})
	req := ExplainRequest{Query: "SELECT * FROM " + orders + " WHERE id = 1"}
	_, err := tools.ExplainQuery(f.ctx, req)
	require.ErrorIs(t, err, ErrUnavailable, "explain.enabled=false")
	require.Contains(t, err.Error(), "explain.enabled")
	planner := plannedOK()
	_, err = tools.ExplainAsAgent(f.ctx, req, planner.plan)
	require.ErrorIs(t, err, ErrUnavailable, "agents honour explain.enabled too")
	require.Equal(t, 0, planner.calls, "a disabled explain reached the planner")

	cfg.Enabled = true // a config reload flips the same struct
	res, err := tools.ExplainQuery(f.ctx, req)
	require.NoError(t, err)
	requirePlanJSON(t, res)
	_, err = tools.ExplainAsAgent(f.ctx, req, planner.plan)
	require.NoError(t, err)
	require.Equal(t, 1, planner.calls)
}

func TestExplainDisabledWinsOverAMissingPool(t *testing.T) {
	tools := New(nil, Options{Explain: &config.ExplainConfig{Enabled: false}})
	_, err := tools.ExplainQuery(context.Background(), ExplainRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, ErrUnavailable)
	require.Contains(t, err.Error(), "explain.enabled")
	// nil options mean the default (enabled): only the missing pool refuses.
	_, err = New(nil, Options{}).ExplainQuery(context.Background(),
		ExplainRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, ErrUnavailable)
	require.Contains(t, err.Error(), "no database pool")
}

func TestExplainAsAgentIsPlanOnly(t *testing.T) {
	f := newFixture(t)
	planner := plannedOK()
	tools := New(f.pool, Options{})
	res, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT * FROM orders " +
		"WHERE id = $1;", Params: []string{"7"}, Analyze: true}, planner.plan)
	require.NoError(t, err)
	require.Equal(t, readapi.ExplainRequest{SQL: "SELECT * FROM orders WHERE id = $1",
		Params: []string{"7"}}, planner.req)
	require.False(t, res.Analyzed)
	require.Nil(t, res.ActualTimeMs)
	require.Equal(t, AgentAnalyzeRefused, res.AnalyzeRefused)
	require.Contains(t, res.Note, "own database role")
	require.Equal(t, readapi.VerdictExecute, res.Verdict)
	require.Equal(t, readapi.StatusOK, res.Status)
	require.Equal(t, samplePlan, string(res.PlanJSON))
	require.InDelta(t, 8.29, res.EstimatedCost, 1e-9)
	require.Len(t, res.NodeBreakdown, 1)
	require.Equal(t, "Index Scan", res.NodeBreakdown[0].NodeType)
	require.Contains(t, res.Summary, "Index Scan")
	require.Contains(t, res.Summary, "not executed")

	res, err = tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT 1"}, planner.plan)
	require.NoError(t, err)
	require.Empty(t, res.AnalyzeRefused, "nothing was refused when analyze was not asked")
}

func TestExplainAsAgentReturnsRefusalsAsResults(t *testing.T) {
	f := newFixture(t)
	tools := New(f.pool, Options{})
	for name, broker := range map[string]readapi.ExplainResult{
		"blocked": {Verdict: readapi.VerdictBlocked, ReasonCode: "agent_classification",
			Detail: "ssn is pii", Fix: "select other columns"},
		"permission denied": {Verdict: readapi.VerdictExecute, Status: readapi.StatusFailed,
			SQLState: "42501", Message: "permission denied", Retryable: false},
	} {
		planner := &recordingPlanner{result: broker}
		res, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT 1"},
			planner.plan)
		require.NoError(t, err, name)
		require.Equal(t, broker.Verdict, res.Verdict, name)
		require.Equal(t, broker.Status, res.Status, name)
		require.Equal(t, broker.ReasonCode, res.ReasonCode, name)
		require.Equal(t, broker.Fix, res.Fix, name)
		require.Equal(t, broker.SQLState, res.SQLState, name)
		require.Empty(t, res.PlanJSON, name)
		require.Empty(t, res.NodeBreakdown, name)
		require.Contains(t, res.Summary, "not planned", name)
	}
}

func TestExplainAsAgentMapsPlannerErrors(t *testing.T) {
	f := newFixture(t)
	tools := New(f.pool, Options{})
	for broker, want := range map[error]error{
		readapi.ErrInvalid:         ErrInvalid,
		readapi.ErrUnavailable:     ErrUnavailable,
		readapi.ErrUnknownDatabase: ErrNotFound,
		readapi.ErrNotPermitted:    ErrInvalid,
		context.Canceled:           context.Canceled,
	} {
		planner := &recordingPlanner{err: broker}
		_, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT 1"}, planner.plan)
		require.ErrorIs(t, err, want, "%v", broker)
	}
	odd := errors.New("socket closed")
	_, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT 1"},
		(&recordingPlanner{err: odd}).plan)
	require.ErrorIs(t, err, odd, "other errors keep their cause")
	_, err = tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT 1"}, nil)
	require.ErrorIs(t, err, ErrUnavailable, "no planner: fail closed")
}

func TestExplainAsAgentRejectsWritesBeforeThePlanner(t *testing.T) {
	f := newFixture(t)
	orders := f.ordersTable(100)
	tools := New(f.pool, Options{})
	planner := plannedOK()
	for _, sql := range []string{"DELETE FROM " + orders, "SELECT 1; DROP TABLE " + orders,
		"   ", "UPDATE " + orders + " SET amount = 0"} {
		_, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: sql}, planner.plan)
		require.ErrorIs(t, err, ErrInvalid, sql)
	}
	_, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{Query: "SELECT 1", QueryID: 5},
		planner.plan)
	require.ErrorIs(t, err, ErrInvalid, "query and query_id")
	require.Equal(t, 0, planner.calls)
	require.Equal(t, int64(100), f.count(orders))
}

func TestExplainAsAgentResolvesQueryIDText(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	orders := f.ordersTable(500)
	tools := New(f.pool, Options{})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.resetOwnStatements()
		f.execArgs("SELECT id FROM "+orders+" WHERE customer_id = $1", 7)
		id, ok := f.statementID(orders + " WHERE customer_id = $1")
		if !ok {
			return []string{"statement missing from pg_stat_statements"}
		}
		planner := plannedOK()
		res, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{QueryID: QueryID(id)},
			planner.plan)
		require.NoError(t, err)
		require.Equal(t, 1, planner.calls)
		require.Contains(t, planner.req.SQL, "customer_id = $1")
		require.Empty(t, planner.req.Params, "a recorded statement is planned generically")
		require.Contains(t, res.Query, "customer_id = $1")
		return nil
	})
	_, err := tools.ExplainAsAgent(f.ctx, ExplainRequest{QueryID: 4242424242424242},
		plannedOK().plan)
	require.ErrorIs(t, err, ErrNotFound)
}
