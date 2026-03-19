package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// requirePG skips the test if SAGE_DATABASE_URL is not set or PG is unreachable.
// It initialises the global pool once and reuses it across tests.
func requirePG(t *testing.T) {
	t.Helper()
	cfg := loadConfig()
	if cfg.DatabaseURL == "" {
		t.Skip("SAGE_DATABASE_URL not set, skipping integration test")
	}
	if pool != nil {
		return
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		t.Skipf("invalid SAGE_DATABASE_URL: %v", err)
	}
	poolCfg.MaxConns = 3

	p, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Skipf("cannot create pool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		p.Close()
		t.Skipf("cannot connect to PostgreSQL: %v", err)
	}
	pool = p
	ensureMCPLogTable()
}

// newTestServer creates an httptest.Server that mirrors the production MCP mux,
// wrapped with rate limiting using the supplied limiter. If rl is nil a generous
// default is used.
func newTestServer(rl *RateLimiter) *httptest.Server {
	if rl == nil {
		rl = NewRateLimiter(600)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", handleSSE)
	mux.HandleFunc("/messages", handleMessages)
	return httptest.NewServer(rateLimitMiddleware(rl, mux))
}

// newPrometheusTestServer creates an httptest.Server for the /metrics endpoint.
func newPrometheusTestServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)
	return httptest.NewServer(mux)
}

// sseEvent is a parsed SSE event.
type sseEvent struct {
	Event string
	Data  string
}

// readSSEEvent reads the next SSE event from a bufio.Scanner.
// Returns nil if the stream ends before a complete event.
func readSSEEvent(scanner *bufio.Scanner) *sseEvent {
	var event, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// End of event
			if event != "" || data != "" {
				return &sseEvent{Event: event, Data: data}
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	return nil
}

// establishSSESession opens an SSE connection, reads the endpoint event, and
// returns the full messages URL plus a function to read SSE events and a cancel func.
func establishSSESession(t *testing.T, ts *httptest.Server) (messagesURL string, readEvent func() *sseEvent, cancel context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse", nil)
	if err != nil {
		cancel()
		t.Fatalf("creating SSE request: %v", err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("SSE GET failed: %v", err)
	}

	scanner := bufio.NewScanner(resp.Body)

	// Read the initial endpoint event.
	ev := readSSEEvent(scanner)
	if ev == nil {
		cancel()
		resp.Body.Close()
		t.Fatal("expected endpoint event from SSE, got nothing")
	}
	if ev.Event != "endpoint" {
		cancel()
		resp.Body.Close()
		t.Fatalf("expected event type 'endpoint', got %q", ev.Event)
	}

	messagesURL = ts.URL + ev.Data
	readEvent = func() *sseEvent { return readSSEEvent(scanner) }
	return messagesURL, readEvent, cancel
}

// sendJSONRPC posts a JSON-RPC request to the messages endpoint and returns
// the HTTP response (the actual RPC response comes on the SSE stream).
func sendJSONRPC(t *testing.T, client *http.Client, messagesURL string, method string, id int, params any) *http.Response {
	t.Helper()

	raw, _ := json.Marshal(params)
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &idRaw,
		Method:  method,
		Params:  raw,
	}
	body, _ := json.Marshal(req)
	resp, err := client.Post(messagesURL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST %s failed: %v", messagesURL, err)
	}
	return resp
}

// readJSONRPCResult reads one SSE message event and unmarshals the JSON-RPC
// response's result field into dst.
func readJSONRPCResult(t *testing.T, readEvent func() *sseEvent, dst any) JSONRPCResponse {
	t.Helper()
	ev := readEvent()
	if ev == nil {
		t.Fatal("expected SSE message event, stream ended")
	}
	if ev.Event != "message" {
		t.Fatalf("expected event 'message', got %q", ev.Event)
	}
	var rpcResp JSONRPCResponse
	if err := json.Unmarshal([]byte(ev.Data), &rpcResp); err != nil {
		t.Fatalf("unmarshalling JSON-RPC response: %v", err)
	}
	if rpcResp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: code=%d message=%s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if dst != nil {
		raw, _ := json.Marshal(rpcResp.Result)
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("unmarshalling result into %T: %v", dst, err)
		}
	}
	return rpcResp
}

// ---------------------------------------------------------------------------
// 1. SSE connection — GET /sse returns text/event-stream with endpoint event
// ---------------------------------------------------------------------------

func TestSSEConnection(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse", nil)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /sse failed: %v", err)
	}
	defer func() {
		cancel()
		resp.Body.Close()
	}()

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected Content-Type text/event-stream, got %q", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	ev := readSSEEvent(scanner)
	if ev == nil {
		t.Fatal("expected endpoint event, got none")
	}
	if ev.Event != "endpoint" {
		t.Fatalf("expected event 'endpoint', got %q", ev.Event)
	}
	if !strings.HasPrefix(ev.Data, "/messages?sessionId=") {
		t.Fatalf("expected endpoint data to start with /messages?sessionId=, got %q", ev.Data)
	}
}

// ---------------------------------------------------------------------------
// 2. MCP initialize handshake
// ---------------------------------------------------------------------------

func TestMCPInitialize(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "initialize", 1, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "test", "version": "0.0.1"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	var result InitializeResult
	readJSONRPCResult(t, readEvent, &result)

	if result.ServerInfo.Name == "" {
		t.Fatal("expected serverInfo.name to be non-empty")
	}
	if result.ServerInfo.Name != "pg_sage-sidecar" {
		t.Fatalf("expected serverInfo.name='pg_sage-sidecar', got %q", result.ServerInfo.Name)
	}
	if result.ProtocolVersion == "" {
		t.Fatal("expected protocolVersion to be non-empty")
	}
}

// ---------------------------------------------------------------------------
// 3. resources/list — verify all 6 resources
// ---------------------------------------------------------------------------

func TestResourcesList(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "resources/list", 1, map[string]any{})
	resp.Body.Close()

	var result ResourcesListResult
	readJSONRPCResult(t, readEvent, &result)

	if len(result.Resources) != 6 {
		t.Fatalf("expected 6 resources, got %d", len(result.Resources))
	}

	expectedURIs := map[string]bool{
		"sage://health":           false,
		"sage://findings":         false,
		"sage://slow-queries":     false,
		"sage://schema/{table}":   false,
		"sage://stats/{table}":    false,
		"sage://explain/{queryid}": false,
	}
	for _, r := range result.Resources {
		if _, ok := expectedURIs[r.URI]; ok {
			expectedURIs[r.URI] = true
		}
	}
	for uri, found := range expectedURIs {
		if !found {
			t.Errorf("missing expected resource URI: %s", uri)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. tools/list — verify all 4 tools
// ---------------------------------------------------------------------------

func TestToolsList(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "tools/list", 1, map[string]any{})
	resp.Body.Close()

	var result ToolsListResult
	readJSONRPCResult(t, readEvent, &result)

	if len(result.Tools) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(result.Tools))
	}

	expectedNames := map[string]bool{
		"diagnose":         false,
		"briefing":         false,
		"suggest_index":    false,
		"review_migration": false,
	}
	for _, tool := range result.Tools {
		if _, ok := expectedNames[tool.Name]; ok {
			expectedNames[tool.Name] = true
		}
	}
	for name, found := range expectedNames {
		if !found {
			t.Errorf("missing expected tool: %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. prompts/list — verify prompts are returned
// ---------------------------------------------------------------------------

func TestPromptsList(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "prompts/list", 1, map[string]any{})
	resp.Body.Close()

	var result PromptsListResult
	readJSONRPCResult(t, readEvent, &result)

	if len(result.Prompts) == 0 {
		t.Fatal("expected at least one prompt, got zero")
	}

	expectedNames := map[string]bool{
		"investigate_slow_query": false,
		"review_schema":         false,
		"capacity_plan":         false,
	}
	for _, p := range result.Prompts {
		if _, ok := expectedNames[p.Name]; ok {
			expectedNames[p.Name] = true
		}
	}
	for name, found := range expectedNames {
		if !found {
			t.Errorf("missing expected prompt: %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. resources/read for sage://health — verify valid JSON
// ---------------------------------------------------------------------------

func TestResourcesReadHealth(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "resources/read", 1, ResourcesReadParams{URI: "sage://health"})
	resp.Body.Close()

	var result ResourcesReadResult
	readJSONRPCResult(t, readEvent, &result)

	if len(result.Contents) == 0 {
		t.Fatal("expected at least one content item")
	}
	content := result.Contents[0]
	if content.URI != "sage://health" {
		t.Fatalf("expected URI sage://health, got %q", content.URI)
	}
	if content.Text == "" {
		t.Fatal("expected non-empty text content")
	}

	// Verify it is valid JSON.
	if !json.Valid([]byte(content.Text)) {
		t.Fatalf("expected valid JSON, got: %s", content.Text)
	}
}

// ---------------------------------------------------------------------------
// 7. resources/read for sage://findings — verify it returns content
// ---------------------------------------------------------------------------

func TestResourcesReadFindings(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "resources/read", 1, ResourcesReadParams{URI: "sage://findings"})
	resp.Body.Close()

	var result ResourcesReadResult
	readJSONRPCResult(t, readEvent, &result)

	if len(result.Contents) == 0 {
		t.Fatal("expected at least one content item")
	}
	content := result.Contents[0]
	if content.URI != "sage://findings" {
		t.Fatalf("expected URI sage://findings, got %q", content.URI)
	}
	if content.Text == "" {
		t.Fatal("expected non-empty text content for findings")
	}
}

// ---------------------------------------------------------------------------
// 8. tools/call for briefing — verify text content
// ---------------------------------------------------------------------------

func TestToolsCallBriefing(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	messagesURL, readEvent, cancel := establishSSESession(t, ts)
	defer cancel()

	resp := sendJSONRPC(t, ts.Client(), messagesURL, "tools/call", 1, ToolsCallParams{
		Name:      "briefing",
		Arguments: json.RawMessage(`{}`),
	})
	resp.Body.Close()

	var result ToolsCallResult
	readJSONRPCResult(t, readEvent, &result)

	if len(result.Content) == 0 {
		t.Fatal("expected at least one content item")
	}
	if result.Content[0].Type != "text" {
		t.Fatalf("expected content type 'text', got %q", result.Content[0].Type)
	}
	if result.Content[0].Text == "" {
		t.Fatal("expected non-empty text in briefing result")
	}
}

// ---------------------------------------------------------------------------
// 9. Rate limiting — send more requests than the limit and verify 429
// ---------------------------------------------------------------------------

func TestRateLimiting(t *testing.T) {
	requirePG(t)

	// Create a rate limiter with a very low limit for testing.
	rl := NewRateLimiter(3)
	ts := newTestServer(rl)
	defer ts.Close()

	messagesURL, _, cancel := establishSSESession(t, ts)
	defer cancel()

	// The SSE GET + the session establishment count as 1 request.
	// Now send enough POST requests to exceed the limit.
	var got429 bool
	for i := 0; i < 10; i++ {
		idRaw := json.RawMessage(fmt.Sprintf("%d", i+1))
		req := JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      &idRaw,
			Method:  "ping",
		}
		body, _ := json.Marshal(req)
		resp, err := ts.Client().Post(messagesURL, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			resp.Body.Close()
			break
		}
		resp.Body.Close()
	}

	if !got429 {
		t.Fatal("expected 429 Too Many Requests after exceeding rate limit")
	}
}

// ---------------------------------------------------------------------------
// 10. Invalid session — POST with bad sessionId, expect error
// ---------------------------------------------------------------------------

func TestInvalidSession(t *testing.T) {
	requirePG(t)
	ts := newTestServer(nil)
	defer ts.Close()

	badURL := ts.URL + "/messages?sessionId=bogus-session-id"
	idRaw := json.RawMessage(`1`)
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &idRaw,
		Method:  "ping",
	}
	body, _ := json.Marshal(req)
	resp, err := ts.Client().Post(badURL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for invalid session, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 11. Prometheus /metrics — verify text/plain with pg_sage_info metric
// ---------------------------------------------------------------------------

func TestPrometheusMetrics(t *testing.T) {
	requirePG(t)
	ts := newPrometheusTestServer()
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected Content-Type starting with text/plain, got %q", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	var fullBody strings.Builder
	for scanner.Scan() {
		fullBody.WriteString(scanner.Text())
		fullBody.WriteString("\n")
	}

	body := fullBody.String()
	if !strings.Contains(body, "pg_sage_info") {
		t.Fatalf("expected pg_sage_info metric in /metrics output, got:\n%s", body)
	}
}
