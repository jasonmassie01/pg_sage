//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// JSON-RPC types (mirroring sidecar's mcp.go)
// ---------------------------------------------------------------------------

type JSONRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *JSONRPCError    `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type ToolsCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ---------------------------------------------------------------------------
// Test prompt definitions
// ---------------------------------------------------------------------------

type TestPrompt struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"`
	Tool        string   `json:"tool"`
	Arguments   any      `json:"arguments"`
	Description string   `json:"description"`
	MustContain []string `json:"must_contain,omitempty"`
	MustNotContain []string `json:"must_not_contain,omitempty"`
	ExpectError bool     `json:"expect_error,omitempty"`
	MaxLatencyMS int     `json:"max_latency_ms,omitempty"`
}

type TestResult struct {
	PromptID     string        `json:"prompt_id"`
	Category     string        `json:"category"`
	Tool         string        `json:"tool"`
	Pass         bool          `json:"pass"`
	Latency      time.Duration `json:"latency_ms"`
	ResponseText string        `json:"response_text,omitempty"`
	Failures     []string      `json:"failures,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type TestReport struct {
	ServerURL    string       `json:"server_url"`
	StartedAt    time.Time    `json:"started_at"`
	FinishedAt   time.Time    `json:"finished_at"`
	TotalTests   int          `json:"total_tests"`
	Passed       int          `json:"passed"`
	Failed       int          `json:"failed"`
	Errors       int          `json:"errors"`
	Results      []TestResult `json:"results"`
}

// ---------------------------------------------------------------------------
// SSE session management
// ---------------------------------------------------------------------------

type sseSession struct {
	messagesURL string
	eventCh     chan sseEvent
	cancelFn    context.CancelFunc
	mu          sync.Mutex
}

type sseEvent struct {
	Event string
	Data  string
}

// establishSSE opens GET /sse, reads the endpoint event, and returns
// a session that can send JSON-RPC requests and receive responses.
func establishSSE(
	ctx context.Context,
	baseURL string,
) (*sseSession, error) {
	sseURL := baseURL + "/sse"

	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("creating SSE request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("SSE GET %s: %w", sseURL, err)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		resp.Body.Close()
		return nil, fmt.Errorf(
			"expected Content-Type text/event-stream, got %q", ct,
		)
	}

	eventCh := make(chan sseEvent, 256)
	sess := &sseSession{
		eventCh:  eventCh,
		cancelFn: cancel,
	}

	// Background goroutine reads SSE events and pushes to channel.
	go func() {
		defer resp.Body.Close()
		defer close(eventCh)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		var event, data string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if event != "" || data != "" {
					eventCh <- sseEvent{Event: event, Data: data}
					event, data = "", ""
				}
				continue
			}
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	// Read the initial endpoint event.
	select {
	case ev := <-eventCh:
		if ev.Event != "endpoint" {
			cancel()
			return nil, fmt.Errorf(
				"expected endpoint event, got %q", ev.Event,
			)
		}
		sess.messagesURL = baseURL + ev.Data
	case <-time.After(10 * time.Second):
		cancel()
		return nil, fmt.Errorf("timeout waiting for endpoint event")
	}

	return sess, nil
}

func (s *sseSession) Close() {
	s.cancelFn()
}

// ---------------------------------------------------------------------------
// MCP protocol: initialize handshake
// ---------------------------------------------------------------------------

func (s *sseSession) Initialize() error {
	_, err := s.sendRPC("initialize", 0, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name": "mcp_test_harness", "version": "1.0.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	// Send initialized notification (no ID = notification).
	body := JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(
		s.messagesURL, "application/json", bytes.NewReader(raw),
	)
	if err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	resp.Body.Close()
	return nil
}

// ---------------------------------------------------------------------------
// Send a JSON-RPC request and wait for the response on SSE
// ---------------------------------------------------------------------------

func (s *sseSession) sendRPC(
	method string,
	id int,
	params any,
) (*JSONRPCResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, _ := json.Marshal(params)
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &idRaw,
		Method:  method,
		Params:  raw,
	}
	body, _ := json.Marshal(req)

	resp, err := http.Post(
		s.messagesURL, "application/json", bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", s.messagesURL, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode != http.StatusAccepted &&
		resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}

	// Wait for SSE message event with the response.
	select {
	case ev, ok := <-s.eventCh:
		if !ok {
			return nil, fmt.Errorf("SSE stream closed")
		}
		if ev.Event != "message" {
			return nil, fmt.Errorf(
				"expected message event, got %q", ev.Event,
			)
		}
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal([]byte(ev.Data), &rpcResp); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		return &rpcResp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for SSE response (30s)")
	}
}

// ---------------------------------------------------------------------------
// Call a tool and return parsed result
// ---------------------------------------------------------------------------

func (s *sseSession) CallTool(
	id int,
	toolName string,
	arguments any,
) (*ToolsCallResult, *JSONRPCError, time.Duration, error) {
	start := time.Now()

	params := map[string]any{
		"name":      toolName,
		"arguments": arguments,
	}

	resp, err := s.sendRPC("tools/call", id, params)
	elapsed := time.Since(start)
	if err != nil {
		return nil, nil, elapsed, err
	}

	if resp.Error != nil {
		return nil, resp.Error, elapsed, nil
	}

	var result ToolsCallResult
	raw, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, elapsed, fmt.Errorf(
			"unmarshal ToolsCallResult: %w", err,
		)
	}
	return &result, nil, elapsed, nil
}

// ---------------------------------------------------------------------------
// Read a resource
// ---------------------------------------------------------------------------

func (s *sseSession) ReadResource(
	id int,
	uri string,
) (string, *JSONRPCError, time.Duration, error) {
	start := time.Now()

	params := map[string]string{"uri": uri}
	resp, err := s.sendRPC("resources/read", id, params)
	elapsed := time.Since(start)
	if err != nil {
		return "", nil, elapsed, err
	}

	if resp.Error != nil {
		return "", resp.Error, elapsed, nil
	}

	var result struct {
		Contents []struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"contents"`
	}
	raw, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", nil, elapsed, fmt.Errorf("unmarshal: %w", err)
	}
	if len(result.Contents) == 0 {
		return "", nil, elapsed, nil
	}
	return result.Contents[0].Text, nil, elapsed, nil
}

// ---------------------------------------------------------------------------
// Validate a single prompt
// ---------------------------------------------------------------------------

func validatePrompt(
	sess *sseSession,
	prompt TestPrompt,
	id int,
) TestResult {
	result := TestResult{
		PromptID: prompt.ID,
		Category: prompt.Category,
		Tool:     prompt.Tool,
		Pass:     true,
	}

	var responseText string
	var rpcErr *JSONRPCError
	var elapsed time.Duration
	var err error

	switch {
	case strings.HasPrefix(prompt.Tool, "resource:"):
		uri := strings.TrimPrefix(prompt.Tool, "resource:")
		responseText, rpcErr, elapsed, err = sess.ReadResource(id, uri)
	default:
		args := prompt.Arguments
		if args == nil {
			args = map[string]any{}
		}
		var toolResult *ToolsCallResult
		toolResult, rpcErr, elapsed, err = sess.CallTool(
			id, prompt.Tool, args,
		)
		if toolResult != nil {
			var texts []string
			for _, c := range toolResult.Content {
				texts = append(texts, c.Text)
			}
			responseText = strings.Join(texts, "\n")

			if toolResult.IsError && !prompt.ExpectError {
				result.Failures = append(result.Failures,
					"tool returned isError=true")
			}
			if !toolResult.IsError && prompt.ExpectError {
				result.Failures = append(result.Failures,
					"expected isError=true but got false")
			}
		}
	}

	result.Latency = elapsed
	result.ResponseText = truncate(responseText, 2000)

	if err != nil {
		result.Error = err.Error()
		result.Pass = false
		return result
	}

	if rpcErr != nil {
		if !prompt.ExpectError {
			result.Failures = append(result.Failures,
				fmt.Sprintf("RPC error %d: %s",
					rpcErr.Code, rpcErr.Message))
		}
		result.ResponseText = rpcErr.Message
	}

	lower := strings.ToLower(responseText)

	for _, needle := range prompt.MustContain {
		if !strings.Contains(lower, strings.ToLower(needle)) {
			result.Failures = append(result.Failures,
				fmt.Sprintf("missing expected: %q", needle))
		}
	}

	for _, needle := range prompt.MustNotContain {
		if strings.Contains(lower, strings.ToLower(needle)) {
			result.Failures = append(result.Failures,
				fmt.Sprintf("found forbidden: %q", needle))
		}
	}

	// Check forbidden patterns in ALL responses.
	forbiddenPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)AIza[a-zA-Z0-9_-]{30,}`),
		regexp.MustCompile(`(?i)sk-[a-zA-Z0-9]{20,}`),
		regexp.MustCompile(`(?i)Bearer\s+[a-zA-Z0-9._-]{20,}`),
		regexp.MustCompile(`(?i)panic:`),
		regexp.MustCompile(`(?i)goroutine \d+`),
		regexp.MustCompile(
			`(?i)postgres://[^:]+:[^@]+@`,
		),
	}
	for _, pat := range forbiddenPatterns {
		if pat.MatchString(responseText) {
			result.Failures = append(result.Failures,
				fmt.Sprintf("forbidden pattern matched: %s",
					pat.String()))
		}
	}

	maxLatency := time.Duration(prompt.MaxLatencyMS) * time.Millisecond
	if maxLatency == 0 {
		maxLatency = 30 * time.Second
	}
	if elapsed > maxLatency {
		result.Failures = append(result.Failures,
			fmt.Sprintf("latency %v exceeds max %v",
				elapsed, maxLatency))
	}

	if len(result.Failures) > 0 {
		result.Pass = false
	}
	return result
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr,
			"Usage: go run mcp_test_harness.go <prompts.json> "+
				"[server_url]\n")
		os.Exit(1)
	}

	promptFile := os.Args[1]
	serverURL := "http://localhost:8080"
	if len(os.Args) >= 3 {
		serverURL = os.Args[2]
	}

	// Load prompts.
	data, err := os.ReadFile(promptFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", promptFile, err)
		os.Exit(1)
	}

	var prompts []TestPrompt
	if err := json.Unmarshal(data, &prompts); err != nil {
		fmt.Fprintf(os.Stderr,
			"Error parsing %s: %v\n", promptFile, err)
		os.Exit(1)
	}

	fmt.Printf("Loaded %d prompts from %s\n", len(prompts), promptFile)
	fmt.Printf("Target server: %s\n", serverURL)
	fmt.Println(strings.Repeat("-", 70))

	// Establish SSE session.
	ctx := context.Background()
	sess, err := establishSSE(ctx, serverURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSE connection failed: %v\n", err)
		os.Exit(1)
	}
	defer sess.Close()

	// Initialize MCP handshake.
	if err := sess.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "MCP initialize failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("MCP session established.")
	fmt.Println(strings.Repeat("-", 70))

	// Run each prompt.
	report := TestReport{
		ServerURL:  serverURL,
		StartedAt:  time.Now(),
		TotalTests: len(prompts),
	}

	for i, prompt := range prompts {
		id := i + 10 // start IDs at 10 to avoid collision with init
		fmt.Printf("[%d/%d] %-12s %-20s %s ... ",
			i+1, len(prompts), prompt.Category,
			prompt.Tool, prompt.Description)

		result := validatePrompt(sess, prompt, id)
		report.Results = append(report.Results, result)

		if result.Pass {
			report.Passed++
			fmt.Printf("PASS (%v)\n", result.Latency.Round(time.Millisecond))
		} else if result.Error != "" {
			report.Errors++
			fmt.Printf("ERROR (%v)\n  %s\n",
				result.Latency.Round(time.Millisecond), result.Error)
		} else {
			report.Failed++
			fmt.Printf("FAIL (%v)\n",
				result.Latency.Round(time.Millisecond))
			for _, f := range result.Failures {
				fmt.Printf("  - %s\n", f)
			}
		}

		// Small delay between requests to avoid rate limiting.
		time.Sleep(200 * time.Millisecond)
	}

	report.FinishedAt = time.Now()

	// Print summary.
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("RESULTS: %d passed, %d failed, %d errors (of %d total)\n",
		report.Passed, report.Failed, report.Errors, report.TotalTests)
	fmt.Printf("Duration: %v\n",
		report.FinishedAt.Sub(report.StartedAt).Round(time.Second))

	if report.Failed > 0 || report.Errors > 0 {
		fmt.Println("\nFAILED/ERROR tests:")
		for _, r := range report.Results {
			if !r.Pass {
				fmt.Printf("  %s [%s/%s]\n",
					r.PromptID, r.Category, r.Tool)
				if r.Error != "" {
					fmt.Printf("    error: %s\n", r.Error)
				}
				for _, f := range r.Failures {
					fmt.Printf("    - %s\n", f)
				}
			}
		}
	}

	// Write JSON report.
	reportFile := strings.TrimSuffix(promptFile, ".json") + "_report.json"
	reportJSON, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportFile, reportJSON, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing report: %v\n", err)
	} else {
		fmt.Printf("\nReport written to %s\n", reportFile)
	}

	if report.Failed > 0 || report.Errors > 0 {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...[truncated]"
}
