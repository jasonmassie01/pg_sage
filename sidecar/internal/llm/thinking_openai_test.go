package llm

import (
	"context"
	"testing"
)

// Product decision: current OpenAI reasoning families (gpt-5*, gpt-6* and
// later; o-series already) reason in plain chat and their reasoning
// tokens count against max_completion_tokens, so they are thinking models
// for budgeting. The non-reasoning "-chat" aliases and gpt-4.x are not.
func TestIsThinkingModel_CurrentOpenAIFamilies(t *testing.T) {
	cases := map[string]bool{
		"gpt-5": true, "gpt-5-mini": true, "gpt-5-nano": true, "gpt-5.1": true,
		"gpt-5.2-pro": true, "gpt-6-luna": true, "GPT-6-Luna": true,
		"openai/gpt-5-mini": true, "gpt-7": true, "o3": true, "o4-mini": true,
		"gpt-5-chat-latest": false, "gpt-5.1-chat": false, "gpt-4o": false,
		"gpt-4.1": false, "gpt-4.5-preview": false, "gpt-3.5-turbo": false,
		"gpt-4o-mini": false, "gpt-50x": false, "my-gpt-5-proxy": false, "": false,
	}
	for model, want := range cases {
		if got := isThinkingModel(model); got != want {
			t.Errorf("isThinkingModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// A thinking OpenAI model's plain chat cap carries the reasoning reserve,
// under max_completion_tokens once adapted.
func TestChat_OpenAIThinkingModelCapAfterAdaptation(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	if _, _, err := c.Chat(context.Background(), "sys", "u", 1000); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := numField(t, f.request(t, 1), "max_completion_tokens"); got != 1000+16384 {
		t.Fatalf("max_completion_tokens = %d, want %d", got, 1000+16384)
	}
}

// The reasoning tokens OpenAI reports in completion_tokens_details are
// returned; a tool turn with reasoning_effort none reports 0.
func TestChatWithTools_OpenAIReasoningUsage(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.reasoning = 300 })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "low", noopLog)
	res, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil))
	if err != nil || res.ReasoningTokens != 300 || res.CompletionTokens != 320 ||
		res.Tokens != 420 {
		t.Fatalf("usage with effort low = %+v (%v)", res, err)
	}
	g := newFakeOpenAI(t, func(f *fakeOpenAI) { f.reasoning = 300 })
	none := wireClient(g.srv.URL, "gpt-6-luna", "", "none", noopLog)
	res, err = none.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		requiredOpts(nil))
	if err != nil || res.ReasoningTokens != 0 || res.Tokens != 120 {
		t.Fatalf("usage with effort none = %+v (%v)", res, err)
	}
}
