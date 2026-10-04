package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Conformance over the real Streamable HTTP transport (an httptest server
// in front of Runtime.HTTPHandler): legacy JSON-RPC POSTs, 2026-07-28
// header validation, status codes and the subscriptions/listen stream.

func httpServer(t *testing.T, server *Server, principal *Principal) *httptest.Server {
	t.Helper()
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "http"},
		server, nil, nil)
	require.NoError(t, err)
	runtime.SetWatchInterval(10 * time.Millisecond)
	handler := runtime.HTTPHandler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal != nil {
			r = r.WithContext(WithPrincipal(r.Context(), *principal))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, url, body string, headers map[string]string) (*http.Response,
	map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var decoded map[string]any
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &decoded), string(raw))
	}
	return resp, decoded
}

func modernHeaders(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": method}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func errorCode(t *testing.T, body map[string]any) float64 {
	t.Helper()
	failure := objectMap(t, body["error"])
	code, _ := failure["code"].(float64)
	return code
}

var viewerPrincipal = Principal{Actor: "user:3", Role: "viewer"}

func TestHTTPLegacyInitializeAndToolsList(t *testing.T) {
	ts := httpServer(t, NewServer(&recordingBackend{}), &viewerPrincipal)
	resp, body := post(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize",`+
		`"params":{"protocolVersion":"2025-06-18","capabilities":{},`+
		`"clientInfo":{"name":"c"}}}`, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	require.Equal(t, "2025-06-18", objectMap(t, body["result"])["protocolVersion"])
	resp, body = post(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		map[string]string{"MCP-Protocol-Version": "2025-06-18"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, objectMap(t, body["result"])["tools"])
}

func TestHTTPLegacyUnsupportedVersionHeaderIs400(t *testing.T) {
	ts := httpServer(t, NewServer(&recordingBackend{}), &viewerPrincipal)
	resp, _ := post(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		map[string]string{"MCP-Protocol-Version": "1999-01-01"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHTTPMethodsAndNotifications(t *testing.T) {
	ts := httpServer(t, NewServer(&recordingBackend{}), &viewerPrincipal)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, err := http.NewRequest(method, ts.URL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, method)
		require.Equal(t, "POST", resp.Header.Get("Allow"), method)
	}
	resp, body := post(t, ts.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		nil)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Nil(t, body)
	resp, body = post(t, ts.URL, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, float64(-32600), errorCode(t, body))
}

func TestHTTPRejectsForeignOrigins(t *testing.T) {
	ts := httpServer(t, NewServer(&recordingBackend{}), &viewerPrincipal)
	resp, _ := post(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{"Origin": "https://evil.example"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	same, _ := post(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{"Origin": ts.URL})
	require.Equal(t, http.StatusOK, same.StatusCode)
	none, _ := post(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil)
	require.Equal(t, http.StatusOK, none.StatusCode)
}

func TestHTTPModernRequestNeedsMatchingHeaders(t *testing.T) {
	ts := httpServer(t, NewServer(&recordingBackend{}), &viewerPrincipal)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_policy",` +
		`"arguments":{},` + modernMeta + `}}`
	cases := []map[string]string{
		{},
		{"Mcp-Method": "tools/call", "Mcp-Name": "get_policy"},
		{"MCP-Protocol-Version": "2025-11-25", "Mcp-Method": "tools/call",
			"Mcp-Name": "get_policy"},
		modernHeaders("tools/list", "get_policy"),
		modernHeaders("tools/call", ""),
		modernHeaders("tools/call", "get_ledger"),
		modernHeaders("tools/call", "=?base64?"+base64.StdEncoding.EncodeToString(
			[]byte("get_ledger"))+"?="),
		// Go clients refuse CR/LF in header values; a non-ASCII plain value
		// is what reaches a server and must be refused.
		modernHeaders("tools/call", "get_pölicy"),
	}
	for i, headers := range cases {
		resp, body := post(t, ts.URL, call, headers)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "case %d", i)
		require.Equal(t, float64(-32020), errorCode(t, body), "case %d", i)
	}
	ok, body := post(t, ts.URL, call, modernHeaders("tools/call", "get_policy"))
	require.Equal(t, http.StatusOK, ok.StatusCode)
	require.Equal(t, "complete", objectMap(t, body["result"])["resultType"])
	encoded, _ := post(t, ts.URL, call, modernHeaders("tools/call",
		"=?base64?"+base64.StdEncoding.EncodeToString([]byte("get_policy"))+"?="))
	require.Equal(t, http.StatusOK, encoded.StatusCode)
}

func TestHTTPModernUnsupportedVersionAndUnknownMethod(t *testing.T) {
	ts := httpServer(t, NewServer(&recordingBackend{}), &viewerPrincipal)
	old := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":` +
		`{"io.modelcontextprotocol/protocolVersion":"2030-01-01"}}}`
	resp, body := post(t, ts.URL, old, map[string]string{
		"MCP-Protocol-Version": "2030-01-01", "Mcp-Method": "tools/list"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, float64(-32022), errorCode(t, body))
	unknown := `{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{` + modernMeta + `}}`
	resp, body = post(t, ts.URL, unknown, modernHeaders("resources/list", ""))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, float64(-32601), errorCode(t, body))
}

func TestHTTPSubscriptionStreamsListChanged(t *testing.T) {
	dir := fleetOf("orders")
	ts := httpServer(t, NewServer(&recordingBackend{}).WithDirectory(dir), &viewerPrincipal)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":11,"method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"toolsListChanged":true}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL,
		strings.NewReader(body))
	require.NoError(t, err)
	for k, v := range modernHeaders("subscriptions/listen", "") {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
	events := sseEvents(resp.Body)
	ack := <-events
	require.Equal(t, "notifications/subscriptions/acknowledged", ack["method"])
	dir.set(DatabaseRef{Name: "billing", ID: 2})
	changed := <-events
	require.Equal(t, "notifications/tools/list_changed", changed["method"])
	meta := objectMap(t, objectMap(t, changed["params"])["_meta"])
	require.Equal(t, float64(11), meta["io.modelcontextprotocol/subscriptionId"])
}

func TestHTTPSubscriptionEndsWithACompletionBeforeTheDeadline(t *testing.T) {
	runtimeDeadline := 1500 * time.Millisecond
	server := NewServer(&recordingBackend{}).WithDirectory(fleetOf("orders"))
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "http"},
		server, nil, nil)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(WithPrincipal(context.Background(), viewerPrincipal),
		runtimeDeadline)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":12,"method":"subscriptions/listen","params":{`+modernMeta+
			`,"notifications":{"toolsListChanged":true}}}`)).WithContext(ctx)
	for k, v := range modernHeaders("subscriptions/listen", "") {
		req.Header.Set(k, v)
	}
	start := time.Now()
	runtime.HTTPHandler().ServeHTTP(recorder, req)
	require.Less(t, time.Since(start), runtimeDeadline, "closed before the deadline")
	events := sseEvents(io.NopCloser(strings.NewReader(recorder.Body.String())))
	require.Equal(t, "notifications/subscriptions/acknowledged", (<-events)["method"])
	final := <-events
	require.Equal(t, float64(12), final["id"])
	require.Equal(t, "complete", objectMap(t, final["result"])["resultType"])
}

func sseEvents(body io.Reader) <-chan map[string]any {
	out := make(chan map[string]any, 16)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var message map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &message) == nil {
				out <- message
			}
		}
	}()
	return out
}
