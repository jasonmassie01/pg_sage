package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// toolServer is an OpenAI-compatible mock that replays scripted replies
// and records every request body. It never reaches a live provider.
type toolServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	calls   atomic.Int32
	replies []func(w http.ResponseWriter)
}

func newToolServer(t *testing.T, replies ...func(w http.ResponseWriter)) *toolServer {
	t.Helper()
	ts := &toolServer{replies: replies}
	ts.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			n := int(ts.calls.Add(1)) - 1
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			ts.mu.Lock()
			ts.bodies = append(ts.bodies, body)
			ts.mu.Unlock()
			if n >= len(ts.replies) {
				n = len(ts.replies) - 1
			}
			ts.replies[n](w)
		}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *toolServer) body(i int) map[string]any {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.bodies[i]
}

func toolReply(content string, calls []map[string]any, tokens int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		msg := map[string]any{"role": "assistant", "content": content}
		if calls != nil {
			msg["tool_calls"] = calls
		}
		reason := "stop"
		if len(calls) > 0 {
			reason = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": reason}},
			"usage":   map[string]int{"total_tokens": tokens},
		})
	}
}

func statusReply(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}
}

func fnCall(id, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args}}
}

func toolClient(url string) *Client {
	return New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "test-key", Model: "m",
		TimeoutSeconds: 5, TokenBudgetDaily: 100000,
	}, noopLog)
}

var evidenceTools = []ToolSpec{{
	Name:        "get_evidence",
	Description: "Return one evidence item by id.",
	Parameters: json.RawMessage(`{"type":"object","properties":` +
		`{"id":{"type":"string"}},"required":["id"]}`),
}}

func userMsgs() []Message {
	return []Message{
		{Role: "system", Content: "Answer in JSON."},
		{Role: "user", Content: "Explain incident X."},
	}
}

func TestChatWithTools_ReturnsToolCalls(t *testing.T) {
	ts := newToolServer(t, toolReply("", []map[string]any{
		fnCall("call_1", "get_evidence", `{"id":"E1"}`)}, 120))
	c := toolClient(ts.srv.URL)

	res, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{MaxTokens: 256})
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.ToolCalls))
	}
	call := res.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "get_evidence" ||
		string(call.Arguments) != `{"id":"E1"}` {
		t.Fatalf("call = %+v", call)
	}
	if res.Tokens != 120 || res.FinishReason != "tool_calls" {
		t.Fatalf("tokens=%d finish=%q", res.Tokens, res.FinishReason)
	}
	if got := c.TokensUsedToday(); got != 120 {
		t.Fatalf("budget charged %d tokens, want 120", got)
	}
}

// The request uses the OpenAI tools wire format, including the
// assistant tool_calls and tool-result messages of a second turn.
func TestChatWithTools_RequestWireFormat(t *testing.T) {
	ts := newToolServer(t, toolReply(`{"summary":"ok"}`, nil, 50))
	c := toolClient(ts.srv.URL)
	msgs := append(userMsgs(),
		Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_1", Name: "get_evidence",
			Arguments: json.RawMessage(`{"id":"E1"}`)}}},
		Message{Role: "tool", ToolCallID: "call_1", Content: "pid=42"},
	)
	res, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		ToolOptions{MaxTokens: 300, ToolChoice: ToolChoiceNone})
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if res.Content != `{"summary":"ok"}` || len(res.ToolCalls) != 0 {
		t.Fatalf("result = %+v", res)
	}
	body := ts.body(0)
	if body["tool_choice"] != "none" || body["max_tokens"] != float64(300) {
		t.Fatalf("tool_choice/max_tokens = %v/%v", body["tool_choice"],
			body["max_tokens"])
	}
	tools := body["tools"].([]any)
	fn := tools[0].(map[string]any)
	if fn["type"] != "function" ||
		fn["function"].(map[string]any)["name"] != "get_evidence" {
		t.Fatalf("tools = %v", tools)
	}
	wire := body["messages"].([]any)
	asst := wire[2].(map[string]any)
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	if tc["type"] != "function" ||
		tc["function"].(map[string]any)["arguments"] != `{"id":"E1"}` {
		t.Fatalf("assistant tool_calls = %v", tc)
	}
	tool := wire[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", tool)
	}
	if _, ok := body["response_format"]; ok {
		t.Error("response_format must not be combined with tools")
	}
}

func TestChatWithTools_InvalidRequest(t *testing.T) {
	ts := newToolServer(t, toolReply("x", nil, 1))
	c := toolClient(ts.srv.URL)
	dup := []ToolSpec{evidenceTools[0], evidenceTools[0]}
	cases := map[string]struct {
		msgs  []Message
		tools []ToolSpec
		opts  ToolOptions
	}{
		"no messages":     {nil, evidenceTools, ToolOptions{}},
		"empty messages":  {[]Message{}, evidenceTools, ToolOptions{}},
		"bad role":        {[]Message{{Role: "root", Content: "x"}}, evidenceTools, ToolOptions{}},
		"tool without id": {[]Message{{Role: "tool", Content: "x"}}, evidenceTools, ToolOptions{}},
		"duplicate tool":  {userMsgs(), dup, ToolOptions{}},
		"bad tool name": {userMsgs(), []ToolSpec{{Name: "get evidence;drop",
			Parameters: json.RawMessage(`{"type":"object"}`)}}, ToolOptions{}},
		"empty tool name": {userMsgs(), []ToolSpec{{Name: ""}}, ToolOptions{}},
		"params not object": {userMsgs(), []ToolSpec{{Name: "t",
			Parameters: json.RawMessage(`[1]`)}}, ToolOptions{}},
		"params malformed": {userMsgs(), []ToolSpec{{Name: "t",
			Parameters: json.RawMessage(`{"type":`)}}, ToolOptions{}},
		"bad tool choice":   {userMsgs(), evidenceTools, ToolOptions{ToolChoice: "always"}},
		"required no tools": {userMsgs(), nil, ToolOptions{ToolChoice: ToolChoiceRequired}},
		"negative tokens":   {userMsgs(), evidenceTools, ToolOptions{MaxTokens: -1}},
	}
	for name, tc := range cases {
		_, err := c.ChatWithTools(context.Background(), tc.msgs, tc.tools, tc.opts)
		if !errors.Is(err, ErrInvalidToolRequest) {
			t.Errorf("%s: want ErrInvalidToolRequest, got %v", name, err)
		}
	}
	if n := ts.calls.Load(); n != 0 {
		t.Fatalf("invalid requests reached the provider %d times", n)
	}
	if c.TokensUsedToday() != 0 {
		t.Fatal("invalid requests were charged to the budget")
	}
}

func TestChatWithTools_TooManyToolsRejected(t *testing.T) {
	c := toolClient("http://127.0.0.1:1")
	tools := make([]ToolSpec, MaxTools+1)
	for i := range tools {
		tools[i] = ToolSpec{Name: "t" + strings.Repeat("x", i%5) +
			string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	if _, err := c.ChatWithTools(context.Background(), userMsgs(), tools,
		ToolOptions{}); !errors.Is(err, ErrInvalidToolRequest) {
		t.Fatalf("%d tools: want ErrInvalidToolRequest, got %v", MaxTools+1, err)
	}
	if _, err := c.ChatWithTools(context.Background(), userMsgs(),
		tools[:MaxTools], ToolOptions{}); errors.Is(err, ErrInvalidToolRequest) {
		t.Fatalf("%d tools rejected as invalid: %v", MaxTools, err)
	}
}

func TestChatWithTools_MalformedToolArguments(t *testing.T) {
	cases := map[string]map[string]any{
		"truncated json": fnCall("c1", "get_evidence", `{"id":"E1"`),
		"not an object":  fnCall("c1", "get_evidence", `["E1"]`),
		"unknown tool":   fnCall("c1", "drop_table", `{}`),
		"empty name":     fnCall("c1", "", `{}`),
		"prose":          fnCall("c1", "get_evidence", `sure! the id is E1`),
	}
	for name, call := range cases {
		ts := newToolServer(t, toolReply("", []map[string]any{call}, 40))
		c := toolClient(ts.srv.URL)
		_, err := c.ChatWithTools(context.Background(), userMsgs(),
			evidenceTools, ToolOptions{})
		if !errors.Is(err, ErrMalformedToolCall) {
			t.Errorf("%s: want ErrMalformedToolCall, got %v", name, err)
		}
		if c.TokensUsedToday() != 40 {
			t.Errorf("%s: malformed reply must still be charged, got %d",
				name, c.TokensUsedToday())
		}
	}
}

// Gemini's compat endpoint sometimes fences JSON arguments; they are
// recovered, not rejected.
func TestChatWithTools_MarkdownWrappedArgumentsRecovered(t *testing.T) {
	ts := newToolServer(t, toolReply("", []map[string]any{
		fnCall("c1", "get_evidence", "```json\n{\"id\": \"E2\"}\n```")}, 30))
	c := toolClient(ts.srv.URL)
	res, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	var args struct{ ID string }
	if err := json.Unmarshal(res.ToolCalls[0].Arguments, &args); err != nil ||
		args.ID != "E2" {
		t.Fatalf("arguments = %s (%v), want id E2", res.ToolCalls[0].Arguments, err)
	}
}

// Providers that omit tool-call IDs get deterministic local IDs, so the
// next turn can still reference them.
func TestChatWithTools_MissingCallIDsAssigned(t *testing.T) {
	ts := newToolServer(t, toolReply("", []map[string]any{
		fnCall("", "get_evidence", `{"id":"E1"}`),
		fnCall("", "get_evidence", `{"id":"E2"}`)}, 30))
	c := toolClient(ts.srv.URL)
	res, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if res.ToolCalls[0].ID == "" || res.ToolCalls[0].ID == res.ToolCalls[1].ID {
		t.Fatalf("assigned IDs = %q, %q", res.ToolCalls[0].ID, res.ToolCalls[1].ID)
	}
}

func TestChatWithTools_TooManyToolCallsRejected(t *testing.T) {
	var calls []map[string]any
	for i := 0; i <= MaxToolCallsPerTurn; i++ {
		calls = append(calls, fnCall("", "get_evidence", `{"id":"E1"}`))
	}
	ts := newToolServer(t, toolReply("", calls, 10))
	c := toolClient(ts.srv.URL)
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if !errors.Is(err, ErrMalformedToolCall) {
		t.Fatalf("%d calls: want ErrMalformedToolCall, got %v",
			MaxToolCallsPerTurn+1, err)
	}
}

func TestChatWithTools_EmptyResponse(t *testing.T) {
	ts := newToolServer(t, toolReply("   ", nil, 12))
	c := toolClient(ts.srv.URL)
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("want ErrEmptyResponse, got %v", err)
	}
}

// A rate limit fails fast (no 1+4+16 s retry ladder) with a typed error,
// so an incident narration can fall back inside its deadline.
func TestChatWithTools_RateLimitedFailsFast(t *testing.T) {
	ts := newToolServer(t, statusReply(http.StatusTooManyRequests))
	c := toolClient(ts.srv.URL)
	start := time.Now()
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("rate limit took %s; tool calls must not retry", elapsed)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
	if c.TokensUsedToday() != 0 {
		t.Fatalf("rate-limited call charged %d tokens", c.TokensUsedToday())
	}
}

func TestChatWithTools_ProviderErrorPropagates(t *testing.T) {
	ts := newToolServer(t, statusReply(http.StatusBadRequest))
	c := toolClient(ts.srv.URL)
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want error naming status 400, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Fatal("400 must not be reported as a rate limit")
	}
}

func TestChatWithTools_TimeoutHonored(t *testing.T) {
	block := make(chan struct{})
	ts := newToolServer(t, func(w http.ResponseWriter) { <-block })
	t.Cleanup(func() { close(block) }) // before srv.Close (LIFO)
	c := toolClient(ts.srv.URL)
	start := time.Now()
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{Timeout: 150 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout of 150ms took %s", elapsed)
	}
}

func TestChatWithTools_DisabledNeverCallsProvider(t *testing.T) {
	ts := newToolServer(t, toolReply("x", nil, 1))
	c := New(&config.LLMConfig{Enabled: false, Endpoint: ts.srv.URL,
		APIKey: "k", Model: "m"}, noopLog)
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{})
	if !errors.Is(err, ErrLLMDisabled) {
		t.Fatalf("want ErrLLMDisabled, got %v", err)
	}
	var nilClient *Client
	if _, err := nilClient.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{}); !errors.Is(err, ErrLLMDisabled) {
		t.Fatalf("nil client: want ErrLLMDisabled, got %v", err)
	}
	if ts.calls.Load() != 0 {
		t.Fatal("disabled client reached the provider")
	}
}

func TestChatWithTools_BudgetExhaustedNoCall(t *testing.T) {
	ts := newToolServer(t, toolReply("x", nil, 1))
	c := toolClient(ts.srv.URL)
	c.budgetResetDay.Store(budgetDay(time.Now()))
	c.tokensUsedToday.Store(100000)
	_, err := c.ChatWithTools(context.Background(), userMsgs(),
		evidenceTools, ToolOptions{MaxTokens: 100})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("want budget error, got %v", err)
	}
	if ts.calls.Load() != 0 {
		t.Fatal("exhausted budget still reached the provider")
	}
}

// Kill switch: disabling the LLM while a tool call is in flight cancels
// it and the caller gets an error, never a late answer.
func TestChatWithTools_ReconfigureCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ts := newToolServer(t, func(w http.ResponseWriter) {
		close(started)
		<-release
	})
	t.Cleanup(func() { close(release) }) // before srv.Close (LIFO)
	c := toolClient(ts.srv.URL)
	errc := make(chan error, 1)
	go func() {
		_, err := c.ChatWithTools(context.Background(), userMsgs(),
			evidenceTools, ToolOptions{})
		errc <- err
	}()
	<-started
	c.Reconfigure(&config.LLMConfig{Enabled: false})
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("in-flight call succeeded after the kill switch")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kill switch did not cancel the in-flight call")
	}
}

// Concurrent callers share one budget: the ledger equals the sum of
// provider-reported usage, with no lost updates.
func TestChatWithTools_ConcurrentBudgetAccounting(t *testing.T) {
	ts := newToolServer(t, toolReply(`{"ok":true}`, nil, 7))
	c := toolClient(ts.srv.URL)
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msgs := []Message{{Role: "user", Content: "q" + string(rune('a'+i))}}
			if _, err := c.ChatWithTools(context.Background(), msgs,
				evidenceTools, ToolOptions{MaxTokens: 64}); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if got := c.TokensUsedToday(); got != 7*n {
		t.Fatalf("tokens = %d, want %d", got, 7*n)
	}
}
