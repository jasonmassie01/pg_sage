package main

import "context"

// registerLLMConfigOwner binds the client registry to the active LLM config,
// tracks the shared client, and registers the registry as the single "llm"
// reconfiguration owner so every client follows live llm.* changes.
func registerLLMConfigOwner() {
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
