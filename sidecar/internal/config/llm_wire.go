package config

import (
	"fmt"
	"strings"
)

// OptimizerAuth inherits unified authentication only when the optimizer uses
// the same provider. Explicit optimizer credentials keep API-key semantics.
func (l LLMConfig) OptimizerAuth() string {
	// The App boundary validates both endpoints against its workspace. A
	// dedicated endpoint/model there continues to use the same OAuth principal.
	if DatabricksAppMode() && l.Auth == "databricks" && l.APIKey == "" && l.OptimizerLLM.APIKey == "" {
		return "databricks"
	}
	if l.OptimizerLLM.APIKey != "" || (l.OptimizerLLM.Endpoint != "" &&
		strings.TrimRight(l.OptimizerLLM.Endpoint, "/") != strings.TrimRight(l.Endpoint, "/")) {
		return "api_key"
	}
	return l.Auth
}

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
	if err := l.ValidateDatabricksApp(); err != nil {
		return err
	}
	if !oneOf(l.Auth, []string{"api_key", "databricks"}) {
		return fmt.Errorf("llm.auth must be api_key or databricks")
	}
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
