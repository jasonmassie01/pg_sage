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

// tunerLLMClients returns the tuner's primary client and its fallback. The
// fallback is nil when it would be the primary itself: retrying a failed
// call on the same client doubles latency and circuit-breaker failures.
func tunerLLMClients(manager *llm.Manager) (primary, fallback *llm.Client) {
	if manager == nil {
		return nil, nil
	}
	primary = manager.ForPurpose("query_tuning")
	if cfg.LLM.OptimizerLLM.FallbackToGeneral && manager.General != nil &&
		manager.General != primary {
		fallback = manager.General
	}
	return primary, fallback
}
