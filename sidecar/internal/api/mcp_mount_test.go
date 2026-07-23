package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestHTTPMCPTransportMountsBehindAPIAuthentication(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Transport = "http"
	var calls atomic.Int32
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	})
	router := NewRouterFullRuntime(
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		&RuntimeDeps{MCPHandler: mcpHandler},
		testMCPAuthentication,
	)

	unauthenticated := postMCPRequest(router, "")
	require.Equal(t, http.StatusUnauthorized, unauthenticated.Code)
	require.Zero(t, calls.Load())

	authenticated := postMCPRequest(router, "test-session")
	require.Equal(t, http.StatusOK, authenticated.Code)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, "application/json", authenticated.Header().Get("Content-Type"))
	require.JSONEq(t,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
		authenticated.Body.String(),
	)
}

func TestHTTPMCPTransportIsNotMountedWhenDisabledOrUsingStdio(t *testing.T) {
	testCases := []struct {
		name      string
		enabled   bool
		transport string
	}{
		{name: "disabled", enabled: false, transport: "http"},
		{name: "stdio", enabled: true, transport: "stdio"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.MCP.Enabled = testCase.enabled
			cfg.MCP.Transport = testCase.transport
			var calls atomic.Int32
			router := NewRouterFullRuntime(
				nil,
				cfg,
				nil,
				nil,
				nil,
				nil,
				&RuntimeDeps{MCPHandler: http.HandlerFunc(func(
					w http.ResponseWriter, _ *http.Request,
				) {
					calls.Add(1)
					w.WriteHeader(http.StatusOK)
				})},
				testMCPAuthentication,
			)

			response := postMCPRequest(router, "test-session")

			require.Equal(t, http.StatusNotFound, response.Code)
			require.Zero(t, calls.Load())
		})
	}
}

func testMCPAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Test-Session") != "test-session" {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, request)
	})
}

func postMCPRequest(
	handler http.Handler, session string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	if session != "" {
		request.Header.Set("X-Test-Session", session)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
