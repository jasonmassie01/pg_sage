package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestRuntimeSelectsConfiguredTransport(t *testing.T) {
	testCases := []struct {
		name          string
		config        config.MCPConfig
		wantRuntime   bool
		wantTransport string
		wantHTTP      bool
	}{
		{
			name: "disabled",
			config: config.MCPConfig{
				Enabled: false, Transport: "stdio",
			},
		},
		{
			name: "stdio",
			config: config.MCPConfig{
				Enabled: true, Transport: "stdio",
			},
			wantRuntime: true, wantTransport: "stdio",
		},
		{
			name: "http",
			config: config.MCPConfig{
				Enabled: true, Transport: "http",
			},
			wantRuntime: true, wantTransport: "http", wantHTTP: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, err := NewRuntime(
				testCase.config,
				NewServer(&recordingBackend{}),
				strings.NewReader(""),
				&bytes.Buffer{},
			)
			require.NoError(t, err)
			if !testCase.wantRuntime {
				require.Nil(t, runtime)
				return
			}
			require.NotNil(t, runtime)
			require.Equal(t, testCase.wantTransport, runtime.Transport())
			if testCase.wantHTTP {
				require.NotNil(t, runtime.HTTPHandler())
			} else {
				require.Nil(t, runtime.HTTPHandler())
			}
		})
	}
}

func TestRuntimeRejectsUnknownTransport(t *testing.T) {
	runtime, err := NewRuntime(
		config.MCPConfig{Enabled: true, Transport: "websocket"},
		NewServer(&recordingBackend{}),
		strings.NewReader(""),
		&bytes.Buffer{},
	)

	require.Nil(t, runtime)
	require.ErrorContains(t, err, "transport")
}

func TestStdioTransportProcessesNewlineDelimitedJSONRPC(t *testing.T) {
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` +
		"\n")
	output := &bytes.Buffer{}
	runtime, err := NewRuntime(
		config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(&recordingBackend{}), input, output,
	)
	require.NoError(t, err)

	require.NoError(t, runtime.Serve(context.Background()))
	scanner := bufio.NewScanner(output)
	require.True(t, scanner.Scan())
	var response protocolResponse
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &response))
	require.Equal(t, "2.0", response.JSONRPC)
	require.Empty(t, response.Error.Code)
	require.False(t, scanner.Scan())
}

func TestStdioTransportStopsOnCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	runtime, err := NewRuntime(
		config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(&recordingBackend{}), reader, &bytes.Buffer{},
	)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = runtime.Serve(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestHTTPTransportServesOnlyPostJSONRPC(t *testing.T) {
	runtime, err := NewRuntime(
		config.MCPConfig{Enabled: true, Transport: "http"},
		NewServer(&recordingBackend{}), nil, nil,
	)
	require.NoError(t, err)
	handler := runtime.HTTPHandler()

	post := httptest.NewRequest(
		http.MethodPost,
		"/mcp",
		strings.NewReader(`{
			"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}
		}`),
	)
	post.Header.Set("Content-Type", "application/json")
	postResponse := httptest.NewRecorder()
	handler.ServeHTTP(postResponse, post)
	require.Equal(t, http.StatusOK, postResponse.Code)
	require.Equal(t, "application/json", postResponse.Header().Get("Content-Type"))
	var protocol protocolResponse
	require.NoError(t, json.Unmarshal(postResponse.Body.Bytes(), &protocol))
	require.Empty(t, protocol.Error.Code)

	get := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	require.Equal(t, http.StatusMethodNotAllowed, getResponse.Code)
}

func TestHTTPTransportPropagatesCancellation(t *testing.T) {
	backend := &recordingBackend{}
	backend.changeFunc = func(ctx context.Context, _ ChangeRequest) (ChangeResult, error) {
		backend.changeContextErr = ctx.Err()
		return ChangeResult{}, ctx.Err()
	}
	runtime, err := NewRuntime(
		config.MCPConfig{Enabled: true, Transport: "http"},
		NewServer(backend), nil, nil,
	)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(
		http.MethodPost,
		"/mcp",
		strings.NewReader(`{
			"jsonrpc":"2.0","id":1,"method":"tools/call",
			"params":{"name":"request_change","arguments":{
				"intent":{"kind":"optimize_query","query_id":991}
			}}
		}`),
	).WithContext(ctx)
	response := httptest.NewRecorder()

	runtime.HTTPHandler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var protocol protocolResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &protocol))
	require.Equal(t, -32800, protocol.Error.Code)
	require.ErrorIs(t, backend.changeContextErr, context.Canceled)
}
