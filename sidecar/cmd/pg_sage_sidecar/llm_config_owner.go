package main

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
