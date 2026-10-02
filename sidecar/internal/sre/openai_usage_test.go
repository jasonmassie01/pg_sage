package sre

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// OpenAI reports reasoning in completion_tokens_details; a tool turn sent
// with reasoning_effort "none" reports 0 and must be recorded as answer
// tokens only, while a reasoning turn splits as reported.
func TestSplitUsage_OpenAIBreakdown(t *testing.T) {
	res := Reservation{Input: 4000, Output: 1000, Reasoning: 8192}
	none := splitUsage(llm.ToolResult{Tokens: 120, PromptTokens: 100,
		CompletionTokens: 20}, res)
	if none != (Usage{Input: 100, Output: 20, Reasoning: 0, Known: true}) {
		t.Fatalf("effort none usage = %+v", none)
	}
	reasoned := splitUsage(llm.ToolResult{Tokens: 420, PromptTokens: 100,
		CompletionTokens: 320, ReasoningTokens: 300}, res)
	if reasoned != (Usage{Input: 100, Output: 20, Reasoning: 300, Known: true}) {
		t.Fatalf("reasoning usage = %+v", reasoned)
	}
}
