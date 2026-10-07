package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Specialist contract revision 1.1.0 over MCP: the open tool documents the
// optional statement scope (query_id, query_hash) and the redacted
// investigator transcript has its own read tool. The contract validates
// the scope; the MCP layer passes it through untouched.

func specialistTool(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range NewServer(&specialistRecordingBackend{}).Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not listed", name)
	return Tool{}
}

func TestSpecialistOpen_DocumentsTheQueryScope(t *testing.T) {
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	require.NoError(t, json.Unmarshal(specialistTool(t,
		"specialist_open_investigation").InputSchema, &schema))
	qid := schema.Properties["query_id"]
	require.NotNil(t, qid)
	require.ElementsMatch(t, []any{"string", "integer"}, qid["type"])
	require.NotEmpty(t, qid["description"])
	hash := schema.Properties["query_hash"]
	require.NotNil(t, hash)
	require.Equal(t, "string", hash["type"])
	require.Equal(t, "^[0-9a-f]{64}$", hash["pattern"])
	require.NotContains(t, schema.Required, "query_id")
	require.NotContains(t, schema.Required, "query_hash")
}

func TestSpecialistOpen_QueryScopeReachesTheContract(t *testing.T) {
	backend := &specialistRecordingBackend{}
	response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
		toolCall("specialist_open_investigation", `{"database":"orders",`+
			`"symptom":{"summary":"slow"},"family":"plan_regression",`+
			`"query_id":9007199254740993}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, 1, backend.calls)
	require.JSONEq(t, `{"symptom":{"summary":"slow"},"family":"plan_regression",`+
		`"query_id":9007199254740993}`, string(backend.args))
	require.Contains(t, string(backend.args), "9007199254740993",
		"a 64-bit queryid must reach the contract exactly")
}

func TestSpecialistTranscriptTool(t *testing.T) {
	tool := specialistTool(t, "specialist_investigation_transcript")
	scope, known := RequiredScope(tool.Name, json.RawMessage(`{}`))
	require.True(t, known)
	require.Equal(t, ScopeRead, scope)
	require.True(t, tool.Annotations.ReadOnlyHint)
	backend := &specialistRecordingBackend{}
	response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
		toolCall(tool.Name, specialistToolArgs[tool.Name]))
	require.Empty(t, response.Error.Code)
	require.Equal(t, tool.Name, backend.tool)
	require.Equal(t, "orders", backend.database)
	for _, bad := range []string{`{"database":"orders"}`,
		`{"database":"orders","investigation_id":"44444444-4444-4444-8444-444444444444",` +
			`"reason":"x"}`, `{"database":"orders","investigation_id":5}`} {
		response = invoke(t, NewServer(backend), agentCtx(ScopeRead), toolCall(tool.Name, bad))
		require.Equal(t, codeInvalidParams, response.Error.Code, bad)
	}
	require.Equal(t, 1, backend.calls)
}
