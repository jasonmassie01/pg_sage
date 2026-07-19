package main

func registerLLMConfigOwner() {
	if configController == nil || llmClient == nil {
		return
	}
	if err := configController.RegisterOwner(llmClient); err != nil {
		logWarn("config", "LLM reconfiguration owner: %v", err)
	}
}
