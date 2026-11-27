package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// security-02 in the production backend: an agent principal's
// explain_query never reaches pg_sage's own explainer; it is planned by
// agent governance's broker as the agent's own role, and fails closed when
// governance cannot serve it. People keep pg_sage's explainer.

// explainTools counts which explain path the backend took.
type explainTools struct {
	*allToolsBackend
	mu     sync.Mutex
	people int
	agents int
	req    agenttools.ExplainRequest
}

func (f *explainTools) ExplainQuery(_ context.Context, r agenttools.ExplainRequest) (
	agenttools.ExplainResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people++
	f.req = r
	return agenttools.ExplainResult{Query: r.Query, Analyzed: r.Analyze}, nil
}

func (f *explainTools) ExplainAsAgent(ctx context.Context, r agenttools.ExplainRequest,
	plan agenttools.AgentPlanner) (agenttools.ExplainResult, error) {
	f.mu.Lock()
	f.agents++
	f.req = r
	f.mu.Unlock()
	res, err := plan(ctx, readapi.ExplainRequest{SQL: r.Query, Params: r.Params})
	if err != nil {
		return agenttools.ExplainResult{}, err
	}
	return agenttools.ExplainResult{Query: r.Query, PlanJSON: res.PlanJSON,
		Verdict: res.Verdict, Status: res.Status}, nil
}

// explainBroker is agent governance's read path with AgentExplain.
type explainBroker struct {
	brokerBackend
	explains int
	explain  readapi.ExplainRequest
	result   readapi.ExplainResult
	fail     error
}

func (b *explainBroker) AgentExplain(_ context.Context, r readapi.ExplainRequest) (
	readapi.ExplainResult, error) {
	b.explains++
	b.explain = r
	return b.result, b.fail
}

func explainBackend(t *testing.T, tools AgentToolBackend,
	broker AgentBrokerBackend) *ProductionBackend {
	t.Helper()
	deps := newProductionFixture().dependencies()
	deps.AgentTools, deps.AgentBroker = tools, broker
	backend, err := NewProductionBackend(deps)
	require.NoError(t, err)
	return backend
}

func agentOn(database string) context.Context {
	ctx := WithPrincipal(context.Background(), agentPrincipal("agp_bbbbbbbbbbbbbbbbbbbb"))
	return WithDatabase(ctx, database)
}

func TestProductionExplainRunsAgentsUnderTheirOwnRole(t *testing.T) {
	tools := &explainTools{allToolsBackend: newAllToolsBackend()}
	broker := &explainBroker{result: readapi.ExplainResult{Verdict: readapi.VerdictExecute,
		Status: readapi.StatusOK, PlanJSON: json.RawMessage(`[{"Plan":{}}]`)}}
	backend := explainBackend(t, tools, broker)
	res, err := backend.ExplainQuery(agentOn("app"), agenttools.ExplainRequest{
		Query: "SELECT * FROM t WHERE a = $1", Params: []string{"7"}, Analyze: true})
	require.NoError(t, err)
	require.Equal(t, 0, tools.people, "an agent reached pg_sage's own explainer")
	require.Equal(t, 1, tools.agents)
	require.Equal(t, 1, broker.explains)
	require.Equal(t, readapi.ExplainRequest{Database: "app",
		SQL: "SELECT * FROM t WHERE a = $1", Params: []string{"7"}}, broker.explain)
	require.True(t, tools.req.Analyze, "the tools decide how to answer analyze")
	require.Equal(t, readapi.VerdictExecute, res.Verdict)
	require.Equal(t, `[{"Plan":{}}]`, string(res.PlanJSON))
}

func TestProductionExplainKeepsPeopleOnPgSagesExplainer(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"operator session": WithDatabase(sessionContext("operator"), "app"),
		"user token":       WithDatabase(userTokenContext(ScopePropose), "app"),
		"library caller":   context.Background(),
	} {
		tools := &explainTools{allToolsBackend: newAllToolsBackend()}
		broker := &explainBroker{}
		backend := explainBackend(t, tools, broker)
		res, err := backend.ExplainQuery(ctx, agenttools.ExplainRequest{Query: "SELECT 1",
			Analyze: true})
		require.NoError(t, err, name)
		require.Equal(t, 1, tools.people, name)
		require.Equal(t, 0, tools.agents, name)
		require.Equal(t, 0, broker.explains, "%s went through the agent broker", name)
		require.True(t, res.Analyzed, name)
	}
}

func TestProductionAgentExplainFailsClosed(t *testing.T) {
	plain := &brokerBackend{} // agent_query only: no AgentExplain
	cases := map[string]struct {
		tools  AgentToolBackend
		broker AgentBrokerBackend
	}{
		"no agent governance":         {newExplainTools(), nil},
		"broker without explain":      {newExplainTools(), plain},
		"tools without agent explain": {newAllToolsBackend(), &explainBroker{}},
	}
	for name, c := range cases {
		backend := explainBackend(t, c.tools, c.broker)
		_, err := backend.ExplainQuery(agentOn("app"), agenttools.ExplainRequest{
			Query: "SELECT 1"})
		require.ErrorIs(t, err, agenttools.ErrUnavailable, name)
		require.Contains(t, err.Error(), "agent's own role", name)
		if tools, ok := c.tools.(*explainTools); ok {
			require.Equal(t, 0, tools.people, "%s fell back to pg_sage's role", name)
		} else {
			require.Equal(t, 0, c.tools.(*allToolsBackend).calls(),
				"%s fell back to pg_sage's role", name)
		}
	}
	_, err := explainBackend(t, nil, &explainBroker{}).ExplainQuery(agentOn("app"),
		agenttools.ExplainRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, ErrProductionDependencyUnavailable, "no coding-agent tools")
}

func TestProductionAgentExplainPropagatesBrokerErrors(t *testing.T) {
	tools := &explainTools{allToolsBackend: newAllToolsBackend()}
	down := errors.New("control database unreachable")
	broker := &explainBroker{fail: down}
	_, err := explainBackend(t, tools, broker).ExplainQuery(agentOn("app"),
		agenttools.ExplainRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, down)
	require.Equal(t, 0, tools.people)
}

func TestProductionAgentExplainNeedsAResolvedDatabase(t *testing.T) {
	tools := &explainTools{allToolsBackend: newAllToolsBackend()}
	broker := &explainBroker{}
	ctx := WithPrincipal(context.Background(), agentPrincipal("agp_bbbbbbbbbbbbbbbbbbbb"))
	_, err := explainBackend(t, tools, broker).ExplainQuery(ctx,
		agenttools.ExplainRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, agenttools.ErrInvalid)
	require.Equal(t, 0, broker.explains, "no database: nothing is planned")
	require.Equal(t, 0, tools.people)
}

// The whole MCP surface: an agent's analyze reaches the agent path, and
// the broker's refusal is a normal result the agent can read.
func TestAgentExplainOverMCPReturnsTheBrokersVerdict(t *testing.T) {
	tools := &explainTools{allToolsBackend: newAllToolsBackend()}
	broker := &explainBroker{result: readapi.ExplainResult{Verdict: readapi.VerdictBlocked,
		ReasonCode: "agent_frozen"}}
	server := NewServer(explainBackend(t, tools, broker)).WithDirectory(fleetOf("app"))
	r := invoke(t, server, agentContext(ScopeRead), toolCall("explain_query",
		`{"query":"SELECT 1","analyze":true}`))
	require.Equal(t, 0, r.Error.Code, r.Error.Message)
	require.Equal(t, "blocked", structuredContent(t, r)["verdict"])
	require.Equal(t, "app", broker.explain.Database)
	require.Equal(t, 0, tools.people)
}

func newExplainTools() *explainTools {
	return &explainTools{allToolsBackend: newAllToolsBackend()}
}
