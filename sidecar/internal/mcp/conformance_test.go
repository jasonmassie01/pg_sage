package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Phase 0 #9: MCP clients send ping and expect an empty result.
func TestPingReturnsEmptyResult(t *testing.T) {
	raw := NewServer(&recordingBackend{}).Handle(context.Background(),
		json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))
	require.JSONEq(t, `{"jsonrpc":"2.0","id":7,"result":{}}`, string(raw))
}

// Notifications (no id, or any notifications/* method) never get a reply.
func TestNotificationsGetNoResponse(t *testing.T) {
	server := NewServer(&recordingBackend{})
	for _, request := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","id":9}`,
		`{"jsonrpc":"2.0","method":"unknown/thing"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
	} {
		raw := server.Handle(context.Background(), json.RawMessage(request))
		require.Nil(t, raw, "request %s got a response: %s", request, string(raw))
	}
}

// A request with an id that is not a notification still gets an error.
func TestUnknownMethodWithIDStillFails(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), context.Background(),
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`)
	require.Equal(t, -32601, response.Error.Code)
	require.Equal(t, json.Number("4"), response.ID)
}

// Tool results carry a content[] text block (the JSON of the result) and
// keep structuredContent for clients that read it.
func TestToolResultCarriesContentArray(t *testing.T) {
	databaseID := int64(42)
	backend := &recordingBackend{policyResult: PolicyResult{
		DatabaseID: &databaseID, Version: 7, Profile: "unattended",
	}}
	response := invoke(t, NewServer(backend), operatorContext(context.Background()), `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"get_policy","arguments":{"database_id":42}}
	}`)
	require.Empty(t, response.Error.Code)
	result := objectMap(t, response.Result)
	content, ok := result["content"].([]any)
	require.True(t, ok, "content is not an array: %#v", result["content"])
	require.Len(t, content, 1)
	block := objectMap(t, content[0])
	require.Equal(t, "text", block["type"])
	text, ok := block["text"].(string)
	require.True(t, ok, "text block is not a string: %#v", block["text"])
	structured, err := json.Marshal(result["structuredContent"])
	require.NoError(t, err)
	require.JSONEq(t, string(structured), text)
	require.Equal(t, json.Number("7"), structuredContent(t, response)["version"])
}

func TestToolSuccessHandlesNilResult(t *testing.T) {
	result := toolSuccess(nil)
	content, ok := result["content"].([]map[string]any)
	require.True(t, ok, "content type %T", result["content"])
	require.Equal(t, "null", content[0]["text"])
}

// Over stdio a notification produces no output line at all: a stray line
// would be read as a response to the next request.
func TestStdioSkipsNotificationsAndAnswersPing(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	output := &bytes.Buffer{}
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(&recordingBackend{}), input, output)
	require.NoError(t, err)
	require.NoError(t, runtime.Serve(context.Background()))
	scanner := bufio.NewScanner(output)
	require.True(t, scanner.Scan())
	require.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{}}`, scanner.Text())
	require.False(t, scanner.Scan(), "unexpected extra line: %q", scanner.Text())
}

func TestHTTPNotificationIsAcceptedWithoutBody(t *testing.T) {
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "http"},
		NewServer(&recordingBackend{}), nil, nil)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	recorder := httptest.NewRecorder()
	runtime.HTTPHandler().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Equal(t, 0, recorder.Body.Len())
}
