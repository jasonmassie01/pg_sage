package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agent_query and agent_whoami (spec §8.2): read scope, routed to the
// brokered read path. Gate outcomes are normal results; failures the model
// can act on are isError results with the §8.1 codes.

func init() {
	allToolsArgs["agent_query"] = `{"sql":"SELECT 1"}`
}

func (b *allToolsBackend) AgentQuery(ctx context.Context, _ readapi.Request) (readapi.Result,
	error) {
	b.hit(ctx, "agent_query")
	return readapi.Result{Verdict: readapi.VerdictExecute}, nil
}

func (b *allToolsBackend) AgentWhoAmI(ctx context.Context) (readapi.WhoAmI, error) {
	b.hit(ctx, "agent_whoami")
	return readapi.WhoAmI{}, nil
}

type brokerBackend struct {
	recordingBackend
	req      readapi.Request
	result   readapi.Result
	err      error
	whoami   readapi.WhoAmI
	whoErr   error
	queries  int
	whoCalls int
}

func (b *brokerBackend) AgentQuery(_ context.Context, req readapi.Request) (readapi.Result,
	error) {
	b.queries++
	b.req = req
	return b.result, b.err
}

func (b *brokerBackend) AgentWhoAmI(context.Context) (readapi.WhoAmI, error) {
	b.whoCalls++
	return b.whoami, b.whoErr
}

func brokerAgentCtx() context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:9", Role: "viewer",
		Kind: KindAgent, Scopes: []Scope{ScopeRead}, PrincipalID: "agp_x"})
}

func findTool(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range NewServer(&brokerBackend{}).Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s is not listed", name)
	return Tool{}
}

func TestAgentBrokerToolsAreListedReadOnly(t *testing.T) {
	for _, name := range []string{"agent_query", "agent_whoami"} {
		tool := findTool(t, name)
		scope, ok := RequiredScope(name, nil)
		require.True(t, ok && scope == ScopeRead, "%s needs the read scope, got %s", name,
			scope)
		require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, name)
	}
	props := objectMap(t, decodeSchema(t, findTool(t, "agent_whoami").InputSchema)["properties"])
	_, hasDatabase := props["database"]
	require.True(t, !hasDatabase, "agent_whoami is fleet-wide")
	props = objectMap(t, decodeSchema(t, findTool(t, "agent_query").InputSchema)["properties"])
	for _, p := range []string{"database", "sql", "params", "max_rows"} {
		_, ok := props[p]
		require.True(t, ok, "agent_query has %s", p)
	}
}

func TestAgentQueryPassesArgumentsAndFencesRows(t *testing.T) {
	v := "ann"
	backend := &brokerBackend{result: readapi.Result{Verdict: readapi.VerdictExecute,
		Status: readapi.StatusOK, Columns: []readapi.Column{{Name: "name", Type: "text"}},
		Rows: [][]*string{{&v}}, RowCount: 1, Masked: []string{}}}
	response := invoke(t, NewServer(backend), brokerAgentCtx(), toolCall("agent_query",
		`{"database":"app","sql":"SELECT name FROM t WHERE id = $1","params":["1", 2, true,`+
			` null],"max_rows":5}`))
	require.Equal(t, 0, response.Error.Code, "agent_query call")
	require.Equal(t, 1, backend.queries)
	require.Equal(t, "app", backend.req.Database)
	require.Equal(t, "SELECT name FROM t WHERE id = $1", backend.req.SQL)
	require.Equal(t, 5, backend.req.MaxRows)
	require.Equal(t, 4, len(backend.req.Params))
	require.Equal(t, json.Number("2"), backend.req.Params[1])
	structured := structuredContent(t, response)
	require.Equal(t, "execute", structured["verdict"])
	result := objectMap(t, response.Result)
	content, _ := result["content"].([]any)
	text, _ := objectMap(t, content[0])["text"].(string)
	require.True(t, strings.HasPrefix(text, `<data label="agent_query_rows">`),
		"rows are fenced as untrusted data: %q", text)
	require.True(t, result["isError"] != true, "a result is not an error")
}

func TestAgentQueryBlockedIsANormalResult(t *testing.T) {
	backend := &brokerBackend{result: readapi.Result{Verdict: readapi.VerdictBlocked,
		ReasonCode: "agent_frozen", Fix: "unfreeze"}}
	response := invoke(t, NewServer(backend), brokerAgentCtx(),
		toolCall("agent_query", `{"database":"app","sql":"SELECT 1"}`))
	require.Equal(t, 0, response.Error.Code, "blocked is a gate outcome, not an error")
	structured := structuredContent(t, response)
	require.Equal(t, "blocked", structured["verdict"])
	require.Equal(t, "agent_frozen", structured["reason_code"])
}

func TestAgentQueryErrorsMapToCodes(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{readapi.ErrInvalid, codeInvalidParams},
		{readapi.ErrNotPermitted, codeNotPermitted},
		{readapi.ErrUnknownDatabase, codeUnknownDatabase},
		{readapi.ErrUnavailable, codeUnavailable},
		{errors.New("boom with secret detail"), codeInternal},
	}
	for _, c := range cases {
		backend := &brokerBackend{err: c.err}
		response := invoke(t, NewServer(backend), brokerAgentCtx(),
			toolCall("agent_query", `{"database":"app","sql":"SELECT 1"}`))
		require.Equal(t, c.code, response.Error.Code, "%v", c.err)
		require.True(t, !strings.Contains(response.Error.Message, "secret"),
			"internal detail leaked: %q", response.Error.Message)
	}
}

func TestAgentQueryRejectsBadArguments(t *testing.T) {
	for _, args := range []string{`{"database":"app"}`, `{"database":"app","sql":5}`,
		`{"database":"app","sql":"SELECT 1","max_rows":"x"}`,
		`{"database":"app","sql":"SELECT 1","params":{}}`,
		`{"database":"app","sql":"SELECT 1","other":1}`} {
		backend := &brokerBackend{}
		response := invoke(t, NewServer(backend), brokerAgentCtx(), toolCall("agent_query", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
		require.Equal(t, 0, backend.queries, "%s reached the broker", args)
	}
}

func TestAgentWhoAmI(t *testing.T) {
	backend := &brokerBackend{whoami: readapi.WhoAmI{Principal: readapi.PrincipalView{
		ID: "agp_x", Name: "bot"}}}
	response := invoke(t, NewServer(backend), brokerAgentCtx(), toolCall("agent_whoami", `{}`))
	require.Equal(t, 0, response.Error.Code, "whoami")
	principal := objectMap(t, structuredContent(t, response)["principal"])
	require.Equal(t, "bot", principal["name"])

	backend = &brokerBackend{whoErr: agentguard.ErrNoPrincipal}
	response = invoke(t, NewServer(backend), context.Background(),
		toolCall("agent_whoami", `{}`))
	require.Equal(t, 0, response.Error.Code, "no principal is a gate outcome")
	structured := structuredContent(t, response)
	require.Equal(t, "blocked", structured["verdict"])
	require.Equal(t, "agent_unsponsored", structured["reason_code"])

	response = invoke(t, NewServer(&brokerBackend{}), brokerAgentCtx(),
		toolCall("agent_whoami", `{"x":1}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, "whoami takes no arguments")
}

func TestAgentBrokerToolsUnavailableWithoutBackend(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), brokerAgentCtx(),
		toolCall("agent_query", `{"database":"app","sql":"SELECT 1"}`))
	require.Equal(t, codeUnavailable, response.Error.Code, "no broker configured")
}

// D9's agent_rate reaches the client as the existing rate_limited code
// (-32011) with retry_after, one rate-limit signal (spec §6.2.2).
func TestAgentQueryRateLimitIsMinus32011WithRetryAfter(t *testing.T) {
	backend := &brokerBackend{result: readapi.Result{Verdict: readapi.VerdictBlocked,
		ReasonCode: "agent_rate", RetryAfterSeconds: 90}}
	response := invoke(t, NewServer(backend), brokerAgentCtx(),
		toolCall("agent_query", `{"database":"app","sql":"SELECT 1"}`))
	require.Equal(t, codeRateLimited, response.Error.Code, "agent_rate maps to -32011")
	failure := objectMap(t, structuredContent(t, response)["error"])
	require.Equal(t, "rate_limited", failure["reason"])
	require.Equal(t, json.Number("90"), failure["retry_after"])
}
