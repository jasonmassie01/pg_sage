package llm

import (
	"context"
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// TestChatWithToolsLive_RealProvider checks the tool-calling wire format
// against a real OpenAI-compatible provider. It is opt-in only.
//
// Run: PG_SAGE_LIVE_LLM=1 GEMINI_API_KEY=... go test -count=1 \
//
//	-run TestChatWithToolsLive ./internal/llm/
func TestChatWithToolsLive_RealProvider(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LLM") != "1" {
		t.Skip("set PG_SAGE_LIVE_LLM=1 and GEMINI_API_KEY to run live LLM tests")
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live LLM test")
	}
	c := New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       "https://generativelanguage.googleapis.com/v1beta/openai",
		APIKey:         key,
		Model:          "gemini-2.5-flash",
		TimeoutSeconds: 60,
	}, noopLog)
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
}
