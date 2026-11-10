package llm

import (
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func optimizerParent() config.LLMConfig {
	return config.LLMConfig{
		Enabled: true, Endpoint: "https://general.example/v1",
		APIKey: "sk-general", Model: "general-model",
	}
}

func TestOptimizerAPIKeyOtherOriginGetsEmptyKey(t *testing.T) {
	parent := optimizerParent()
	for _, endpoint := range []string{
		"https://optimizer.example/v1",
		"http://general.example/v1",       // scheme differs
		"https://general.example:8443/v1", // port differs
	} {
		parent.OptimizerLLM = config.OptimizerLLMConfig{Enabled: true, Endpoint: endpoint}
		if got := OptimizerAPIKey(parent); got != "" {
			t.Errorf("optimizer at %s inherited the general key", endpoint)
		}
		c := NewOptimizerClient(&parent, &parent.OptimizerLLM, noopLog)
		if c.cfg.APIKey != "" || c.cfg.Endpoint != endpoint {
			t.Errorf("client for %s: endpoint %q key set=%v", endpoint,
				c.cfg.Endpoint, c.cfg.APIKey != "")
		}
	}
}

func TestOptimizerAPIKeySameOriginInherits(t *testing.T) {
	parent := optimizerParent()
	for _, endpoint := range []string{
		"", "https://general.example/v2/reasoning", "https://GENERAL.example:443/v1",
	} {
		parent.OptimizerLLM = config.OptimizerLLMConfig{Enabled: true, Endpoint: endpoint}
		if got := OptimizerAPIKey(parent); got != "sk-general" {
			t.Errorf("optimizer at %q got %q, want the general key", endpoint, got)
		}
		c := NewOptimizerClient(&parent, &parent.OptimizerLLM, noopLog)
		if c.cfg.APIKey != "sk-general" {
			t.Errorf("client for %q did not inherit the general key", endpoint)
		}
	}
}

func TestOptimizerAPIKeyOwnKeyWins(t *testing.T) {
	parent := optimizerParent()
	parent.OptimizerLLM = config.OptimizerLLMConfig{
		Endpoint: "https://optimizer.example/v1", APIKey: "sk-optimizer",
	}
	if got := OptimizerAPIKey(parent); got != "sk-optimizer" {
		t.Fatalf("own key = %q, want sk-optimizer", got)
	}
}

func TestOptimizerAPIKeyZeroValues(t *testing.T) {
	if got := OptimizerAPIKey(config.LLMConfig{}); got != "" {
		t.Fatalf("zero config key = %q, want empty", got)
	}
	parent := config.LLMConfig{APIKey: "sk-general"} // no general endpoint
	parent.OptimizerLLM.Endpoint = "https://optimizer.example"
	if got := OptimizerAPIKey(parent); got != "" {
		t.Fatalf("no general endpoint: key inherited to %s", parent.OptimizerLLM.Endpoint)
	}
}

func TestOptimizerAPIKeyConcurrentWarnings(t *testing.T) {
	parent := optimizerParent()
	parent.OptimizerLLM.Endpoint = "https://optimizer-concurrent.example/v1"
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if OptimizerAPIKey(parent) != "" {
				t.Error("key inherited across origins")
			}
		}()
	}
	wg.Wait()
}
