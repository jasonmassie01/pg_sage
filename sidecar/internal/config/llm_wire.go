package config

import "fmt"

// Provider wire-shape settings (OpenAI compatibility). Current OpenAI
// models reject max_tokens (they want max_completion_tokens) and reject
// function tools in chat completions unless reasoning_effort is "none".
// "auto" lets the LLM client adapt once per endpoint and model when the
// provider says so; the other values pin the request shape.
const (
	LLMTokenParameterAuto                = "auto"
	LLMTokenParameterMaxTokens           = "max_tokens"
	LLMTokenParameterMaxCompletionTokens = "max_completion_tokens"

	LLMToolReasoningEffortAuto   = "auto"
	LLMToolReasoningEffortNone   = "none"
	LLMToolReasoningEffortLow    = "low"
	LLMToolReasoningEffortMedium = "medium"
	LLMToolReasoningEffortHigh   = "high"
	LLMToolReasoningEffortOmit   = "omit"

	DefaultLLMTokenParameter      = LLMTokenParameterAuto
	DefaultLLMToolReasoningEffort = LLMToolReasoningEffortAuto
)

var (
	llmTokenParameters = []string{LLMTokenParameterAuto,
		LLMTokenParameterMaxTokens, LLMTokenParameterMaxCompletionTokens}
	llmToolReasoningEfforts = []string{LLMToolReasoningEffortAuto,
		LLMToolReasoningEffortNone, LLMToolReasoningEffortLow,
		LLMToolReasoningEffortMedium, LLMToolReasoningEffortHigh,
		LLMToolReasoningEffortOmit}
)

// validateWire checks llm.token_parameter and llm.tool_reasoning_effort.
// Empty means auto (a config built without defaults).
func (l LLMConfig) validateWire() error {
	if !oneOf(l.TokenParameter, llmTokenParameters) {
		return fmt.Errorf("llm.token_parameter %q must be one of %v",
			l.TokenParameter, llmTokenParameters)
	}
	if !oneOf(l.ToolReasoningEffort, llmToolReasoningEfforts) {
		return fmt.Errorf("llm.tool_reasoning_effort %q must be one of %v",
			l.ToolReasoningEffort, llmToolReasoningEfforts)
	}
	return nil
}

func oneOf(value string, allowed []string) bool {
	if value == "" {
		return true
	}
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}
