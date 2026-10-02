package llm

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Live provider tests. Opt-in only (PG_SAGE_LIVE_LLM=1). The provider is
// Gemini's OpenAI-compatible endpoint unless SAGE_LLM_ENDPOINT and
// SAGE_LLM_MODEL name another; the key is SAGE_LLM_API_KEY, else
// GEMINI_API_KEY.
//
// Run: PG_SAGE_LIVE_LLM=1 SAGE_LLM_API_KEY=... [SAGE_LLM_ENDPOINT=...
//
//	SAGE_LLM_MODEL=...] go test -count=1 -run 'Live' ./internal/llm/
func liveConfig(t *testing.T) *config.LLMConfig {
	t.Helper()
	if os.Getenv("PG_SAGE_LIVE_LLM") != "1" {
		t.Skip("set PG_SAGE_LIVE_LLM=1 and SAGE_LLM_API_KEY to run live LLM tests")
	}
	key := firstEnv("SAGE_LLM_API_KEY", "GEMINI_API_KEY")
	if key == "" {
		t.Skip("SAGE_LLM_API_KEY / GEMINI_API_KEY not set; skipping live LLM test")
	}
	return &config.LLMConfig{
		Enabled: true,
		Endpoint: envOr("SAGE_LLM_ENDPOINT",
			"https://generativelanguage.googleapis.com/v1beta/openai"),
		APIKey:         key,
		Model:          envOr("SAGE_LLM_MODEL", "gemini-2.5-flash"),
		TimeoutSeconds: 60,
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func liveLog(t *testing.T) func(string, string, ...any) {
	return func(component, format string, args ...any) {
		t.Logf("[%s] "+format, append([]any{component}, args...)...)
	}
}

// TestChatWithToolsLive_RealProvider checks the tool-calling wire format
// (and, for current OpenAI models, both wire adaptations) against a real
// OpenAI-compatible provider.
func TestChatWithToolsLive_RealProvider(t *testing.T) {
	c := New(liveConfig(t), liveLog(t))
	msgs := []Message{
		{Role: "system", Content: "Use the get_evidence tool for E1."},
		{Role: "user", Content: "What does evidence E1 say?"},
	}
	res, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		ToolOptions{MaxTokens: 1024, ToolChoice: ToolChoiceRequired})
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if len(res.ToolCalls) == 0 || res.ToolCalls[0].Name != "get_evidence" {
		t.Fatalf("want a get_evidence call, got %+v", res)
	}
	t.Logf("usage: total=%d prompt=%d completion=%d reasoning=%d", res.Tokens,
		res.PromptTokens, res.CompletionTokens, res.ReasoningTokens)
	// The second turn answers from the tool result with the adapted shape.
	msgs = append(msgs, Message{Role: "assistant", ToolCalls: res.ToolCalls},
		Message{Role: "tool", ToolCallID: res.ToolCalls[0].ID,
			Content: `{"id":"E1","text":"lock held by pid 42 for 90s"}`})
	final, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		ToolOptions{MaxTokens: 1024, ToolChoice: ToolChoiceNone})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if !strings.Contains(final.Content, "42") {
		t.Fatalf("answer does not use the tool result: %q", final.Content)
	}
	t.Logf("usage turn 2: total=%d prompt=%d completion=%d reasoning=%d",
		final.Tokens, final.PromptTokens, final.CompletionTokens, final.ReasoningTokens)
}

// TestChatLive_RealProvider checks plain chat (json_mode on) live.
func TestChatLive_RealProvider(t *testing.T) {
	cfg := liveConfig(t)
	cfg.JSONMode = true
	c := New(cfg, liveLog(t))
	out, tokens, err := c.Chat(context.Background(),
		`Reply with JSON {"ok":true} only.`, "Health check.", 256)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(StripJSON(out, JSONObject), `"ok"`) || tokens <= 0 {
		t.Fatalf("reply %q (%d tokens)", out, tokens)
	}
	t.Logf("chat tokens=%d", tokens)
}
