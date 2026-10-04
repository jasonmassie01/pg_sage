package main

import "github.com/pg-sage/sidecar/internal/llm"

// newStandaloneLLMManager builds the standalone Manager with the dedicated,
// registry-tracked optimizer client when optimizer_llm is enabled. It used
// to be built with a nil optimizer, so query_tuning silently ran on the
// general client (G3-B15).
func newStandaloneLLMManager(general *llm.Client) *llm.Manager {
	var optimizerClient *llm.Client
	if cfg.LLM.OptimizerLLM.Enabled {
		optimizerClient = llmClients.newClient(llmRoleOptimizer, "", true)
	}
	return llm.NewManager(
		general, optimizerClient, cfg.LLM.OptimizerLLM.FallbackToGeneral,
	)
}
