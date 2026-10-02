package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Reasoning allowance (Sage SRE M3): a caller may budget a thinking
// model's reasoning explicitly (ToolOptions.ReasoningTokens) instead of
// the hard-coded 16384 reserve. Zero keeps today's behaviour for every
// existing caller; non-thinking models never get a reasoning reserve.

func modelClient(url, model string, daily int) *Client {
	return New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "k", Model: model,
		TimeoutSeconds: 5, TokenBudgetDaily: daily}, noopLog)
}

func sentMaxTokens(t *testing.T, ts *toolServer) int {
	t.Helper()
	v, ok := ts.body(0)["max_tokens"].(float64)
	if !ok {
		t.Fatalf("request has no max_tokens: %v", ts.body(0))
	}
	return int(v)
}

func TestChatWithTools_ReasoningAllowance(t *testing.T) {
	cases := []struct {
		model     string
		reasoning int
		want      int
	}{
		{"gemini-2.5-flash", 8192, 2000 + 8192},
		{"gemini-3-pro-preview", 8192, 2000 + 8192},
		{"o3-mini", 8192, 2000 + 8192},
		{"deepseek-r1", 8192, 2000 + 8192},
		{"gemini-2.5-flash", 0, 2000 + 16384}, // unchanged for existing callers
		{"o4-mini", 0, 2000 + 16384},
		{"gpt-4o-mini", 8192, 2000}, // non-thinking: the allowance adds nothing
		{"gpt-4o-mini", 0, 2000},
	}
	for _, c := range cases {
		ts := newToolServer(t, toolReply("ok", nil, 50))
		_, err := modelClient(ts.srv.URL, c.model, 1_000_000).ChatWithTools(
			context.Background(), userMsgs(), evidenceTools,
			ToolOptions{MaxTokens: 2000, ReasoningTokens: c.reasoning})
		if err != nil {
			t.Fatalf("%s/%d: %v", c.model, c.reasoning, err)
		}
		if got := sentMaxTokens(t, ts); got != c.want {
			t.Errorf("%s with reasoning %d: max_tokens = %d, want %d", c.model,
				c.reasoning, got, c.want)
		}
	}
}

// The default completion cap (no MaxTokens) plus an allowance.
func TestChatWithTools_ReasoningAllowanceWithDefaultCap(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 50))
	_, err := modelClient(ts.srv.URL, "gemini-2.5-pro", 1_000_000).ChatWithTools(
		context.Background(), userMsgs(), nil, ToolOptions{ReasoningTokens: 1024})
	if err != nil || sentMaxTokens(t, ts) != 16384+1024 {
		t.Fatalf("max_tokens = %d (%v), want %d", sentMaxTokens(t, ts), err, 16384+1024)
	}
}

func TestChatWithTools_NegativeReasoningRejected(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 50))
	_, err := modelClient(ts.srv.URL, "gemini-2.5-flash", 1_000_000).ChatWithTools(
		context.Background(), userMsgs(), nil, ToolOptions{ReasoningTokens: -1})
	if !errors.Is(err, ErrInvalidToolRequest) || ts.calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d, want ErrInvalidToolRequest and no request", err,
			ts.calls.Load())
	}
}

// The per-call budget is charged the prompt plus answer and reasoning.
func TestChatWithTools_ReasoningCountsAgainstThePerCallBudget(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 50))
	client := modelClient(ts.srv.URL, "gemini-2.5-flash", 1_000_000)
	tight := &ledgerBudget{limit: 2000 + 8192}
	_, err := client.ChatWithTools(context.Background(), userMsgs(), nil,
		ToolOptions{MaxTokens: 2000, ReasoningTokens: 8192, Budget: tight})
	if !errors.Is(err, ErrBudgetExhausted) || ts.calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d, want the call refused before dispatch", err,
			ts.calls.Load())
	}
	room := &ledgerBudget{limit: 2000 + 8192 + 1000}
	if _, err := client.ChatWithTools(context.Background(), userMsgs(), nil,
		ToolOptions{MaxTokens: 2000, ReasoningTokens: 8192, Budget: room}); err != nil {
		t.Fatalf("with room for prompt, answer and reasoning: %v", err)
	}
}

// The daily budget still refuses a call whose answer plus reasoning does
// not fit.
func TestChatWithTools_DailyBudgetCountsReasoning(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 50))
	_, err := modelClient(ts.srv.URL, "gemini-2.5-flash", 5000).ChatWithTools(
		context.Background(), userMsgs(), nil,
		ToolOptions{MaxTokens: 2000, ReasoningTokens: 8192})
	if !errors.Is(err, ErrBudgetExhausted) || ts.calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d, want the daily budget to refuse", err, ts.calls.Load())
	}
}

func usageReply(usage map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant",
				"content": "ok"}, "finish_reason": "stop"}}, "usage": usage})
	}
}

// The usage breakdown the provider reports is returned as reported.
func TestChatWithTools_UsageBreakdown(t *testing.T) {
	ts := newToolServer(t, usageReply(map[string]any{"prompt_tokens": 3000,
		"completion_tokens": 500, "total_tokens": 18500,
		"completion_tokens_details": map[string]any{"reasoning_tokens": 15000}}),
		usageReply(map[string]any{"total_tokens": 77}))
	client := modelClient(ts.srv.URL, "gemini-2.5-flash", 1_000_000)
	res, err := client.ChatWithTools(context.Background(), userMsgs(), nil,
		ToolOptions{MaxTokens: 2000, ReasoningTokens: 8192})
	if err != nil || res.Tokens != 18500 || res.PromptTokens != 3000 ||
		res.CompletionTokens != 500 || res.ReasoningTokens != 15000 {
		t.Fatalf("result = %+v (%v)", res, err)
	}
	res, err = client.ChatWithTools(context.Background(), []Message{
		{Role: "user", Content: "second, different prompt"}}, nil, ToolOptions{})
	if err != nil || res.Tokens != 77 || res.PromptTokens != 0 ||
		res.CompletionTokens != 0 || res.ReasoningTokens != 0 {
		t.Fatalf("total-only usage = %+v (%v)", res, err)
	}
}

func TestIsThinkingModel_Exported(t *testing.T) {
	for model, want := range map[string]bool{"gemini-2.5-flash": true,
		"gemini-3-pro-preview": true, "o3-mini": true, "openai/o1": true,
		"deepseek-r1": true, "deepseek-reasoner": true, "gpt-4o-mini": false,
		"gemini-1.5-flash": false, "llama-3.1-70b": false, "": false} {
		if got := IsThinkingModel(model); got != want {
			t.Errorf("IsThinkingModel(%q) = %v, want %v", model, got, want)
		}
	}
	c := modelClient("http://127.0.0.1:1", "gemini-2.5-flash", 1)
	if !c.ThinkingModel() {
		t.Fatal("client with gemini-2.5-flash is not a thinking model")
	}
	c.Reconfigure(&config.LLMConfig{Enabled: true, Model: "gpt-4o-mini"})
	if c.ThinkingModel() {
		t.Fatal("ThinkingModel ignores a reconfigured model")
	}
	var nilClient *Client
	if nilClient.ThinkingModel() {
		t.Fatal("a nil client reports a thinking model")
	}
}
