package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// registerLLMConfigOwner binds the client registry to the active LLM config,
// tracks the shared client, and registers the registry as the single "llm"
// reconfiguration owner so every client follows live llm.* changes.
func registerLLMConfigOwner() {
	registerLLMRedactionOwner()
	if configController == nil || llmClient == nil {
		return
	}
	llmClients.bind(configController.Active().Config.LLM)
	llmClients.add(llmClient, llmRoleGeneral, "", true)
	if err := configController.RegisterOwner(llmClients); err != nil {
		logWarn("config", "LLM reconfiguration owner: %v", err)
	}
}

// learnLLMInputLimits runs the shared client's one configure-time model
// listing in the background (bounded to 5 s) so the pre-flight prompt
// size guard knows the model's input limit (llm-05). It lists only where
// the listing reports limits; startup never waits for it.
func learnLLMInputLimits() {
	if llmClient == nil {
		return
	}
	ctx := shutdownCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go llmClient.LearnInputLimits(ctx)
}

// registerLLMRedactionOwner applies llm.redact_numeric_literals from the
// active config and registers its live owner (llm-09). It runs whether or
// not an LLM client exists: the switch is process-wide.
func registerLLMRedactionOwner() {
	if configController == nil {
		return
	}
	var active *config.LLMConfig
	if cfg := configController.Active().Config; cfg != nil {
		active = &cfg.LLM
	}
	llm.ApplyRedactionConfig(active) // nil fails closed to on
	if err := configController.RegisterOwner(llm.RedactionOwner{}); err != nil {
		logWarn("config", "LLM redaction owner: %v", err)
	}
}
