package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// security-02: for people (sessions and user tokens) explain_query needs
// the propose scope, plan-only and ANALYZE alike, which is REST's operator
// minimum for POST /api/v1/explain. Agents keep the read scope; their
// EXPLAIN runs plan-only under their own role (production_explain_test.go).
// No concurrent access tests: scope resolution is a pure function of the
// tool, its arguments and the principal.

var explainCalls = []string{`{"query":"SELECT 1"}`, `{"query":"SELECT 1","analyze":true}`,
	`{"query_id":42}`}

func sessionContext(role string) context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "user:7", Role: role})
}

func userTokenContext(scopes ...Scope) context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:u1", Role: "operator",
		Kind: KindHuman, Scopes: scopes, TokenID: "u1"})
}

func TestExplainQueryNeedsProposeForPeople(t *testing.T) {
	for _, args := range append(explainCalls, `{}`, ``) {
		scope, ok := RequiredScope("explain_query", json.RawMessage(args))
		require.True(t, ok, "explain_query is a known tool")
		require.Equal(t, ScopePropose, scope, "people need propose for %s", args)
		for _, p := range []Principal{{}, {Actor: "user:1", Role: "viewer"},
			{Actor: "token:u", Kind: KindHuman, Scopes: []Scope{ScopeRead}}} {
			scope, ok = RequiredScopeFor(p, "explain_query", json.RawMessage(args))
			require.True(t, ok)
			require.Equal(t, ScopePropose, scope, "%+v %s", p, args)
		}
	}
}

func TestExplainQueryScopeForAgentsIsRead(t *testing.T) {
	agent := Principal{Actor: "token:agt1", Kind: KindAgent, Scopes: []Scope{ScopeRead}}
	for _, args := range append(explainCalls, `{}`) {
		scope, ok := RequiredScopeFor(agent, "explain_query", json.RawMessage(args))
		require.True(t, ok)
		require.Equal(t, ScopeRead, scope, "agents explain with read: %s", args)
	}
	// Every other tool keeps one scope whoever calls it.
	for _, tool := range []string{"top_queries", "whatif_index", "decide_fact",
		"propose_fact", "agent_query"} {
		want, _ := RequiredScope(tool, nil)
		got, ok := RequiredScopeFor(agent, tool, nil)
		require.True(t, ok, tool)
		require.Equal(t, want, got, tool)
	}
	_, ok := RequiredScopeFor(agent, "execute_sql", nil)
	require.False(t, ok, "an unknown tool has no scope for anyone")
}

func TestViewerCannotExplainOverMCP(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"viewer session":    sessionContext("viewer"),
		"read user token":   userTokenContext(ScopeRead),
		"unknown role":      sessionContext("intern"),
		"unbound (no auth)": context.Background(),
	} {
		for _, args := range explainCalls {
			b := newAllToolsBackend()
			r := invoke(t, b.server(), ctx, toolCall("explain_query", args))
			require.Equal(t, codeScopeRequired, r.Error.Code, "%s %s", name, args)
			require.Contains(t, r.Error.Message, "propose scope required", name)
			require.Equal(t, 0, b.calls(), "%s reached the explainer with %s", name, args)
		}
	}
}

func TestOperatorExplainReachesTheExplainer(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"operator session":   sessionContext("operator"),
		"admin session":      sessionContext("admin"),
		"propose user token": userTokenContext(ScopeRead, ScopePropose),
	} {
		b := newAllToolsBackend()
		r := invoke(t, b.server(), ctx, toolCall("explain_query",
			`{"query":"SELECT 1","analyze":true}`))
		require.Equal(t, 0, r.Error.Code, "%s: %s", name, r.Error.Message)
		require.Equal(t, 1, b.calls(), name)
		got := b.agent.req.(agenttools.ExplainRequest)
		require.Equal(t, agenttools.ExplainRequest{Query: "SELECT 1", Analyze: true}, got)
	}
}

func TestAgentWithReadScopeStillExplains(t *testing.T) {
	b := newAllToolsBackend()
	r := invoke(t, b.server(), agentContext(ScopeRead), toolCall("explain_query",
		`{"query":"SELECT 1"}`))
	require.Equal(t, 0, r.Error.Code, r.Error.Message)
	require.Equal(t, "explain_query", b.tool)
	require.Equal(t, 1, b.calls())
	none := newAllToolsBackend()
	r = invoke(t, none.server(), agentContext(), toolCall("explain_query",
		`{"query":"SELECT 1"}`))
	require.Equal(t, codeScopeRequired, r.Error.Code, "an agent without read")
	require.Equal(t, 0, none.calls())
}

// listedTools is the tool names tools/list shows the caller.
func listedTools(t *testing.T, ctx context.Context) map[string]bool {
	t.Helper()
	r := invoke(t, newAllToolsBackend().server(), ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	require.Equal(t, 0, r.Error.Code, r.Error.Message)
	tools, _ := objectMap(t, r.Result)["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		name, _ := objectMap(t, tool)["name"].(string)
		names[name] = true
	}
	require.True(t, names["list_databases"], "tools/list returned no read tools")
	return names
}

func TestToolsListShowsExplainOnlyToThoseWhoMayCallIt(t *testing.T) {
	require.False(t, listedTools(t, sessionContext("viewer"))["explain_query"],
		"a viewer is shown a tool it may not call")
	require.False(t, listedTools(t, userTokenContext(ScopeRead))["explain_query"])
	require.True(t, listedTools(t, sessionContext("operator"))["explain_query"])
	require.True(t, listedTools(t, agentContext(ScopeRead))["explain_query"],
		"agents keep plan-only explain with read")
}

func TestToolReferenceDocumentsExplainScopes(t *testing.T) {
	text := ToolReferenceMarkdown(NewServer(&recordingBackend{}).Tools())
	start := strings.Index(text, "### `explain_query`")
	require.True(t, start >= 0, "explain_query is documented")
	section := text[start:]
	section = section[:strings.Index(section[4:], "\n### ")+4]
	require.Contains(t, section, "Scope: `propose` (operator) for people; `read` for "+
		"agents, plan-only, under the agent's role")
	description := toolByName(t, "explain_query").Description
	require.Contains(t, description, "plan-only")
	require.Contains(t, description, "own database role")
}
