package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Current OpenAI models reject function tools in /v1/chat/completions
// unless reasoning_effort is "none". In llm.tool_reasoning_effort=auto the
// client retries such a 400 once with reasoning_effort "none" and
// remembers it per (endpoint, model). Combined with the token-parameter
// adaptation, the first tool call to gpt-6-luna needs two adaptations.

func requiredOpts(b Budgeter) ToolOptions {
	return ToolOptions{MaxTokens: 2000, ToolChoice: ToolChoiceRequired, Budget: b}
}

func TestChatWithTools_AutoAdaptsBothParameters(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr, f.toolEffortErr = openAIMaxTokensErr, openAIToolEffortErr
	})
	logs := &capLog{}
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", logs.log)
	ledger := &ledgerBudget{limit: 1_000_000}
	res, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(ledger))
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "get_evidence" {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	if res.Tokens != 120 || res.PromptTokens != 100 || res.CompletionTokens != 20 ||
		res.ReasoningTokens != 0 {
		t.Fatalf("usage = %+v, want 120 total, 100/20, 0 reasoning", res)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("requests = %d, want 3", f.calls.Load())
	}
	if keys := f.keys(t, 1); strings.Contains(keys, "reasoning_effort") ||
		!strings.Contains(keys, "max_completion_tokens") {
		t.Fatalf("second request keys = %s", keys)
	}
	last := f.request(t, 2)
	if last["reasoning_effort"] != "none" || last["tool_choice"] != "required" {
		t.Fatalf("third request = %v", last)
	}
	if numField(t, last, "max_completion_tokens") != numField(t, f.request(t, 0),
		"max_tokens") {
		t.Fatal("the completion cap changed while adapting")
	}
	if net, _ := ledger.snapshot(); net != 120 || c.TokensUsedToday() != 120 {
		t.Fatalf("budgets charged per-call=%d daily=%d, want 120 once", net,
			c.TokensUsedToday())
	}
	if failureCount(c) != 0 {
		t.Fatalf("adaptations counted as %d breaker failures", failureCount(c))
	}
	if logs.count("max_completion_tokens") != 1 || logs.count("reasoning_effort") != 1 {
		t.Fatalf("want one log line per adaptation: %v", logs.lines)
	}
}

// Once learned, tool calls go out adapted; plain Chat on the same model
// uses max_completion_tokens but never reasoning_effort.
func TestChatWithTools_AdaptationRemembered(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr, f.toolEffortErr = openAIMaxTokensErr, openAIToolEffortErr
	})
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	if _, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil)); err != nil {
		t.Fatalf("first: %v", err)
	}
	msgs := []Message{{Role: "user", Content: "a second incident"}}
	if _, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		requiredOpts(nil)); err != nil {
		t.Fatalf("second: %v", err)
	}
	if f.calls.Load() != 4 {
		t.Fatalf("requests = %d, want 4 (3 + 1 adapted)", f.calls.Load())
	}
	fourth := f.request(t, 3)
	if fourth["reasoning_effort"] != "none" || fourth["max_completion_tokens"] == nil {
		t.Fatalf("remembered tool request = %v", fourth)
	}
	if _, _, err := c.Chat(context.Background(), "sys", "plain", 100); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if keys := f.keys(t, 4); keys != "max_completion_tokens,messages,model" {
		t.Fatalf("plain chat keys after tool adaptation = %s", keys)
	}
}

// A provider that accepts max_tokens but needs reasoning_effort for tools
// gets exactly one retry, keeping max_tokens.
func TestChatWithTools_EffortOnlyAdaptation(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.toolEffortErr = openAIToolEffortErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	if _, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil)); err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if keys := f.keys(t, 1); keys !=
		"max_tokens,messages,model,reasoning_effort,tool_choice,tools" {
		t.Fatalf("retry keys = %s", keys)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", f.calls.Load())
	}
}

// The reasoning-effort error is only adapted for requests with tools.
func TestChatWithTools_EffortErrorWithoutToolsIsAFailure(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.next = func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(openAIToolEffortErr))
		}
	})
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	_, err := c.ChatWithTools(context.Background(), userMsgs(), nil, ToolOptions{})
	if err == nil || f.calls.Load() != 1 || failureCount(c) != 1 {
		t.Fatalf("err=%v requests=%d failures=%d, want one failure", err,
			f.calls.Load(), failureCount(c))
	}
}

func TestChatWithTools_ExplicitReasoningEffort(t *testing.T) {
	for _, effort := range []string{"none", "low", "medium", "high"} {
		f := newFakeOpenAI(t, nil)
		c := wireClient(f.srv.URL, "gpt-6-luna", "", effort, noopLog)
		if _, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
			requiredOpts(nil)); err != nil {
			t.Fatalf("%s: %v", effort, err)
		}
		if got := f.request(t, 0)["reasoning_effort"]; got != effort {
			t.Fatalf("reasoning_effort = %v, want %s", got, effort)
		}
	}
	// An explicit effort the model rejects is not overridden.
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.toolEffortErr = openAIToolEffortErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "low", noopLog)
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil))
	if err == nil || f.calls.Load() != 1 {
		t.Fatalf("err=%v requests=%d, want the explicit effort's rejection", err,
			f.calls.Load())
	}
}

// omit never sends reasoning_effort and never adapts it.
func TestChatWithTools_OmitReasoningEffort(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.toolEffortErr = openAIToolEffortErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "max_completion_tokens", "omit", noopLog)
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil))
	if err == nil || !strings.Contains(err.Error(), "reasoning_effort") {
		t.Fatalf("err = %v, want the provider's rejection", err)
	}
	if f.calls.Load() != 1 || f.request(t, 0)["reasoning_effort"] != nil {
		t.Fatalf("requests=%d first=%v", f.calls.Load(), f.request(t, 0))
	}
}

// A 429 on the adapted retry is still ErrRateLimited (no retry ladder).
func TestChatWithTools_RateLimitDuringAdaptation(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr = openAIMaxTokensErr
		f.next = func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	ledger := &ledgerBudget{limit: 1_000_000}
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(ledger))
	if !errors.Is(err, ErrRateLimited) || f.calls.Load() != 2 {
		t.Fatalf("err=%v requests=%d, want ErrRateLimited after 2", err, f.calls.Load())
	}
	if net, _ := ledger.snapshot(); net != 0 || c.TokensUsedToday() != 0 {
		t.Fatalf("failed call left a charge: per-call=%d daily=%d", net,
			c.TokensUsedToday())
	}
}

// In auto mode a provider that accepts the classic shape (Gemini's
// OpenAI-compatible endpoint) gets an unchanged tool request.
func TestChatWithTools_AutoModeLeavesGeminiUnchanged(t *testing.T) {
	f := newFakeOpenAI(t, nil)
	c := wireClient(f.srv.URL, "gemini-2.5-flash", "auto", "auto", noopLog)
	if _, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil)); err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if keys := f.keys(t, 0); keys != "max_tokens,messages,model,tool_choice,tools" {
		t.Fatalf("Gemini tool request keys = %s", keys)
	}
}

// The thinking-model reasoning allowance still sizes the cap after the
// parameter is renamed.
func TestChatWithTools_ReasoningAllowanceAfterAdaptation(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	opts := requiredOpts(nil)
	opts.ReasoningTokens = 4096
	if _, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		opts); err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if got := numField(t, f.request(t, 1), "max_completion_tokens"); got != 2000+4096 {
		t.Fatalf("max_completion_tokens = %d, want %d", got, 2000+4096)
	}
}
