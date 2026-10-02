package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// pg_sage_optimizer_enabled reports whether the index optimizer can run:
// llm.optimizer.enabled now defaults to true, so the config value alone
// would read 1 on every deployment without an LLM.
func TestOptimizerGaugeReportsEffectiveState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoint  string
		optimizer bool
		noManager bool
		want      int
	}{
		{"defaults, no llm", "", true, false, 0},
		{"defaults, llm configured", "http://127.0.0.1:1/v1", true, false, 1},
		{"optimizer off, llm configured", "http://127.0.0.1:1/v1", false, false, 0},
		{"no llm manager", "http://127.0.0.1:1/v1", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveFleetRuntimeGlobals(t)
			cfg = config.DefaultConfig()
			cfg.LLM.Optimizer.Enabled = tc.optimizer
			if tc.endpoint != "" {
				cfg.LLM.Endpoint, cfg.LLM.APIKey = tc.endpoint, "fixture-key"
			}
			llmClient = llm.New(&cfg.LLM, nil)
			llmMgr = newStandaloneLLMManager(llmClient)
			if tc.noManager {
				llmMgr = nil
			}
			if got := optimizerGaugeValue(); got != tc.want {
				t.Fatalf("optimizerGaugeValue() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A manager without a general client must not panic the metrics page.
func TestOptimizerGaugeToleratesNilGeneralClient(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	llmMgr = llm.NewManager(nil, nil, false)
	if got := optimizerGaugeValue(); got != 0 {
		t.Fatalf("optimizerGaugeValue() = %d with no client, want 0", got)
	}
}
