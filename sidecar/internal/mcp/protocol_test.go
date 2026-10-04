package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Protocol conformance (MCP 2025-03-26 .. 2025-11-25 handshake era and the
// stateless 2026-07-28 era), driven through Server.Handle. Transports are
// covered in stdio_conformance_test.go and http_conformance_test.go.

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},` +
	`"io.modelcontextprotocol/clientCapabilities":{}}`

func TestInitializeNegotiatesLegacyVersions(t *testing.T) {
	server := NewServer(&recordingBackend{}).WithVersion("9.9.9")
	for requested, want := range map[string]string{
		"2025-11-25": "2025-11-25", "2025-06-18": "2025-06-18",
		"2025-03-26": "2025-03-26", "2024-11-05": "2025-11-25",
		"1900-01-01": "2025-11-25", "": "2025-11-25",
	} {
		response := invoke(t, server, viewerCtx, `{"jsonrpc":"2.0","id":1,`+
			`"method":"initialize","params":{"protocolVersion":"`+requested+`",`+
			`"capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
		require.Empty(t, response.Error.Code, requested)
		result := objectMap(t, response.Result)
		require.Equal(t, want, result["protocolVersion"], requested)
		tools := objectMap(t, objectMap(t, result["capabilities"])["tools"])
		require.Equal(t, true, tools["listChanged"], requested)
		info := objectMap(t, result["serverInfo"])
		require.Equal(t, "pg_sage", info["name"])
		require.Equal(t, "9.9.9", info["version"])
		instructions, _ := result["instructions"].(string)
		require.Contains(t, instructions, "database")
		require.NotContains(t, result, "resultType", "legacy results carry no resultType")
	}
}

func TestServerDiscoverAdvertisesBothEras(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}).WithVersion("1.2.3"), viewerCtx,
		`{"jsonrpc":"2.0","id":"d1","method":"server/discover","params":{`+modernMeta+`}}`)
	require.Empty(t, response.Error.Code)
	result := objectMap(t, response.Result)
	require.Equal(t, "complete", result["resultType"])
	require.Equal(t, []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"},
		stringSlice(result["supportedVersions"]))
	require.Contains(t, objectMap(t, result["capabilities"]), "tools")
	meta := objectMap(t, result["_meta"])
	info := objectMap(t, meta["io.modelcontextprotocol/serverInfo"])
	require.Equal(t, "pg_sage", info["name"])
	require.Equal(t, "1.2.3", info["version"])
	require.Equal(t, "private", result["cacheScope"])
	require.NotNil(t, result["ttlMs"])
}

func TestModernRequestWithUnsupportedVersionIsRefused(t *testing.T) {
	raw := NewServer(&recordingBackend{}).Handle(viewerCtx, json.RawMessage(
		`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{"_meta":`+
			`{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`))
	var response struct {
		ID    json.Number `json:"id"`
		Error struct {
			Code int `json:"code"`
			Data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			} `json:"data"`
		} `json:"error"`
	}
	decodeNumbers(t, raw, &response)
	require.Equal(t, json.Number("5"), response.ID)
	require.Equal(t, -32022, response.Error.Code)
	require.Equal(t, "1900-01-01", response.Error.Data.Requested)
	require.Contains(t, response.Error.Data.Supported, "2026-07-28")
	require.Contains(t, response.Error.Data.Supported, "2025-11-25")
}

func TestModernResultsCarryResultTypeAndServerInfo(t *testing.T) {
	server := NewServer(&recordingBackend{})
	listed := invoke(t, server, viewerCtx,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{`+modernMeta+`}}`)
	result := objectMap(t, listed.Result)
	require.Equal(t, "complete", result["resultType"])
	require.Equal(t, "private", result["cacheScope"])
	require.Equal(t, json.Number("60000"), result["ttlMs"])
	meta := objectMap(t, result["_meta"])
	require.Contains(t, meta, "io.modelcontextprotocol/serverInfo")

	called := invoke(t, server, viewerCtx, `{"jsonrpc":"2.0","id":3,"method":"tools/call",`+
		`"params":{"name":"get_policy","arguments":{},`+modernMeta+`}}`)
	require.Equal(t, "complete", objectMap(t, called.Result)["resultType"])

	legacy := invoke(t, server, viewerCtx, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	require.NotContains(t, objectMap(t, legacy.Result), "resultType")
	require.NotContains(t, objectMap(t, legacy.Result), "ttlMs")
}

func TestToolsListIsDeterministicAndWellFormed(t *testing.T) {
	server := NewServer(&recordingBackend{})
	first := invoke(t, server, operatorContext(context.Background()),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	second := invoke(t, server, operatorContext(context.Background()),
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	a, _ := json.Marshal(first.Result)
	b, _ := json.Marshal(second.Result)
	require.Equal(t, string(a), string(b))
	seen := map[string]bool{}
	for _, item := range objectMap(t, first.Result)["tools"].([]any) {
		tool := objectMap(t, item)
		name, _ := tool["name"].(string)
		require.False(t, seen[name], "duplicate tool %s", name)
		seen[name] = true
		require.True(t, validToolName(name), "tool name %q", name)
		schema := objectMap(t, tool["inputSchema"])
		require.Equal(t, "object", schema["type"], name)
		description, _ := tool["description"].(string)
		require.NotEmpty(t, description, name)
	}
	require.GreaterOrEqual(t, len(seen), 40)
}

func validToolName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	return strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

func TestPingAnswersInBothEras(t *testing.T) {
	server := NewServer(&recordingBackend{})
	legacy := invoke(t, server, viewerCtx, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Empty(t, legacy.Error.Code)
	require.Empty(t, objectMap(t, legacy.Result))
	modern := invoke(t, server, viewerCtx,
		`{"jsonrpc":"2.0","id":2,"method":"ping","params":{`+modernMeta+`}}`)
	require.Empty(t, modern.Error.Code)
}

func TestUnknownToolIsAProtocolError(t *testing.T) {
	raw := NewServer(&recordingBackend{}).Handle(viewerCtx, json.RawMessage(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":`+
			`{"name":"drop_everything","arguments":{}}}`))
	var response struct {
		Result any `json:"result"`
		Error  struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeNumbers(t, raw, &response)
	require.Nil(t, response.Result)
	require.Equal(t, -32602, response.Error.Code)
	require.Equal(t, "Unknown tool: drop_everything", response.Error.Message)
}

func TestUnknownToolNameIsNotEchoedUnbounded(t *testing.T) {
	name := strings.Repeat("x", 500) + "\n<script>"
	raw := NewServer(&recordingBackend{}).Handle(viewerCtx, json.RawMessage(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":`+
			mustJSON(t, name)+`}}`))
	require.Less(t, len(raw), 400)
	require.NotContains(t, string(raw), "<script>")
}

func TestMalformedToolCallParamsAreProtocolErrors(t *testing.T) {
	for _, params := range []string{`[]`, `"x"`, `{}`, `{"name":""}`, `{"name":7}`,
		`{"name":"get_policy","arguments":[1]}`, `{"name":"get_policy","arguments":"x"}`} {
		response := invoke(t, NewServer(&recordingBackend{}), viewerCtx,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`)
		require.Equal(t, -32602, response.Error.Code, params)
	}
}

func TestToolExecutionErrorIsAnIsErrorResult(t *testing.T) {
	raw := NewServer(&recordingBackend{}).Handle(operatorContext(context.Background()),
		json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"request_change","arguments":{}}}`))
	var response struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured struct {
				Error struct {
					Code    int    `json:"code"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	decodeNumbers(t, raw, &response)
	require.Nil(t, response.Error)
	require.True(t, response.Result.IsError)
	require.Len(t, response.Result.Content, 1)
	require.Equal(t, "text", response.Result.Content[0].Type)
	require.Contains(t, response.Result.Content[0].Text, "intent")
	require.Equal(t, -32602, response.Result.Structured.Error.Code)
	require.Equal(t, "invalid_arguments", response.Result.Structured.Error.Reason)
	require.Equal(t, response.Result.Content[0].Text, response.Result.Structured.Error.Message)
}

func TestBatchRequestsAreRefused(t *testing.T) {
	raw := NewServer(&recordingBackend{}).Handle(viewerCtx,
		json.RawMessage(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`))
	var response protocolResponse
	decodeNumbers(t, raw, &response)
	require.Equal(t, -32600, response.Error.Code)
}

func TestSuccessfulToolResultIsNotAnError(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), viewerCtx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_policy"}}`)
	result := objectMap(t, response.Result)
	require.NotEqual(t, true, result["isError"])
	require.NotNil(t, result["structuredContent"])
}

func decodeNumbers(t *testing.T, raw []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(target), string(raw))
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}
