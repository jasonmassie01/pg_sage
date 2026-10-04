package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Coding-agent tools (roadmap phase 3): typed arguments only, validated
// before the backend; read tools for any bound principal, propose tools
// with the propose scope; backend errors map to distinguishable codes.

func callAgentTool(t *testing.T, b *allToolsBackend, name, args string) protocolResponse {
	t.Helper()
	return invoke(t, b.server(), agentContext(ScopeRead, ScopePropose), toolCall(name, args))
}

func TestAgentToolsAreRegisteredWithStrictSchemas(t *testing.T) {
	for _, name := range []string{"list_databases", "top_queries", "explain_query",
		"whatif_index", "lint_migration", "query_sources", "mark_object",
		"get_source_fix_packet", "report_source_fix"} {
		schema := decodeSchema(t, toolByName(t, name).InputSchema)
		require.Equal(t, false, schema["additionalProperties"], name)
	}
	require.Equal(t, []string{"finding_id"},
		stringSlice(decodeSchema(t, toolByName(t, "get_source_fix_packet").InputSchema)["required"]))
	require.Equal(t, []string{"finding_id", "stage"},
		stringSlice(decodeSchema(t, toolByName(t, "report_source_fix").InputSchema)["required"]))
}

func TestAgentToolArgumentsReachTheBackendTyped(t *testing.T) {
	b := newAllToolsBackend()
	r := callAgentTool(t, b, "top_queries",
		`{"limit":5,"order_by":"mean_time","include_plans":false}`)
	require.Empty(t, r.Error.Code, r.Error.Message)
	require.Equal(t, agenttools.TopQueriesRequest{Limit: 5, OrderBy: "mean_time",
		IncludePlans: false}, b.agent.req)
	callAgentTool(t, b, "top_queries", `{}`)
	require.Equal(t, agenttools.TopQueriesRequest{IncludePlans: true}, b.agent.req)

	callAgentTool(t, b, "explain_query", `{"query_id":"-8765432109876543210","analyze":true}`)
	explain := b.agent.req.(agenttools.ExplainRequest)
	require.Equal(t, agenttools.QueryID(-8765432109876543210), explain.QueryID)
	require.True(t, explain.Analyze)

	callAgentTool(t, b, "whatif_index", `{"ddl":"CREATE INDEX ON public.t (a)",`+
		`"query_ids":[12,"-3"]}`)
	require.Equal(t, []agenttools.QueryID{12, -3}, b.agent.req.(agenttools.WhatIfRequest).QueryIDs)

	callAgentTool(t, b, "mark_object", `{"subject_kind":"table","subject":"public.orders",`+
		`"mark":"exempt","repo":"acme/app","path":"db/migrate","evidence":"owned by rails"}`)
	require.Equal(t, agenttools.MarkRequest{Kind: "table", Subject: "public.orders",
		Mark: "exempt", Repo: "acme/app", Path: "db/migrate", Evidence: "owned by rails"},
		b.agent.req)
	require.Equal(t, "mcp:token:agt1", b.agent.actor)
}

func TestReportSourceFixArgumentsAreTyped(t *testing.T) {
	b := newAllToolsBackend()
	r := callAgentTool(t, b, "report_source_fix", `{"finding_id":7,"stage":"deployed",`+
		`"commit":"0a1b2c3d","deployed_at":"2026-10-04T10:00:00Z","packet_hash":"abc"}`)
	require.Empty(t, r.Error.Code, r.Error.Message)
	got := b.agent.req.(agenttools.ReportRequest)
	require.Equal(t, int64(7), got.FindingID)
	require.Equal(t, "deployed", got.Stage)
	require.Equal(t, "0a1b2c3d", got.Commit)
	require.Equal(t, "abc", got.PacketHash)
	require.True(t, got.DeployedAt.Equal(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)))
	require.Equal(t, "mcp:token:agt1", b.agent.actor)
}

func TestAgentToolsRejectInvalidArguments(t *testing.T) {
	cases := map[string][]string{
		"top_queries": {`{"limit":"5"}`, `{"order_by":7}`, `{"bogus":1}`},
		"explain_query": {`{}`, `{"query":"SELECT 1","query_id":5}`, `{"query":""}`,
			`{"query_id":"12abc"}`, `{"query_id":"99999999999999999999999"}`,
			`{"query":"SELECT 1","params":"x"}`},
		"whatif_index":   {`{}`, `{"ddl":""}`, `{"ddl":"CREATE INDEX i ON t(a)","query_ids":"1"}`},
		"lint_migration": {`{}`, `{"sql":"  "}`, `{"sql":"x","pg_version":"17"}`},
		"query_sources":  {`{"sample_seconds":"1"}`, `{"query_id":true}`},
		"mark_object": {`{}`, `{"subject_kind":"index","subject":"i","mark":"owned"}`,
			`{"subject_kind":"slot","subject":"s","mark":"owned","evidence":"e"}`,
			`{"subject_kind":"index","subject":"i","mark":"delete","evidence":"e"}`},
		"get_source_fix_packet": {`{}`, `{"finding_id":0}`, `{"finding_id":-4}`,
			`{"finding_id":"7"}`},
		"report_source_fix": {`{"finding_id":7}`, `{"finding_id":7,"stage":"merged"}`,
			`{"finding_id":7,"stage":"pr_opened","pr_url":"javascript:alert(1)"}`,
			`{"finding_id":7,"stage":"pr_opened","pr_url":"http://github.com/a/b/pull/1"}`,
			`{"finding_id":7,"stage":"deployed","commit":"NOT-HEX"}`,
			`{"finding_id":7,"stage":"deployed","deployed_at":"yesterday"}`,
			`{"finding_id":7,"stage":"pr_opened","pr_url":"https://x.test/\u0000"}`},
	}
	for name, argsList := range cases {
		for _, args := range argsList {
			b := newAllToolsBackend()
			r := callAgentTool(t, b, name, args)
			require.Equal(t, -32602, r.Error.Code, "%s %s", name, args)
			require.Zero(t, b.calls(), "%s %s", name, args)
		}
	}
}

func TestAgentToolBackendErrorsAreDistinguishable(t *testing.T) {
	cases := map[error]int{
		fmt.Errorf("%w: no such finding", agenttools.ErrNotFound):      -32004,
		fmt.Errorf("%w: limit", agenttools.ErrInvalid):                 -32602,
		fmt.Errorf("%w: hypopg", agenttools.ErrUnavailable):            -32010,
		agenttools.ErrNoChange:                                         -32602,
		fmt.Errorf("%w: already verified", agenttools.ErrTransition):   -32009,
		fmt.Errorf("wrap: %w", facts.ErrProtectedSubject):              -32602,
		context.DeadlineExceeded:                                       -32800,
		errors.New("pq: password authentication failed for user root"): -32603,
	}
	for err, code := range cases {
		b := newAllToolsBackend()
		b.agentErr = err
		r := callAgentTool(t, b, "get_source_fix_packet", `{"finding_id":7}`)
		require.Equal(t, code, r.Error.Code, err.Error())
		require.NotContains(t, r.Error.Message, "password", err.Error())
	}
}

func TestAgentToolsUnavailableWithoutBackend(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), viewerCtx,
		toolCall("top_queries", `{}`))
	require.Equal(t, -32603, response.Error.Code)
	require.Contains(t, response.Error.Message, "unavailable")
}

func TestQueryIDsAreStringsInResults(t *testing.T) {
	raw, err := agenttools.QueryID(-8765432109876543210).MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, `"-8765432109876543210"`, string(raw))
}
