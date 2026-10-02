package optimizer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm.optimizer.enabled now defaults to on. With config.DefaultConfig()
// and a configured provider the optimizer actually asks the LLM; the
// default llm block alone (no endpoint or key) never reaches a provider.

func countingOptimizerLLM(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_, _ = w.Write(fnTestChatJSON("[]", 120))
		}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func defaultOptimizer(client *llm.Client) *Optimizer {
	defaults := config.DefaultConfig()
	opt := New(client, nil, nil, &defaults.LLM.Optimizer, 160000,
		defaults.LLM.OptimizerLLM.MaxOutputTokens, fnNoopLog)
	unavailable := false
	opt.hypopg.available = &unavailable
	return opt
}

func TestOptimizerDefaultsCallConfiguredLLM(t *testing.T) {
	defaults := config.DefaultConfig()
	if !defaults.LLM.Optimizer.Enabled {
		t.Fatal("precondition: llm.optimizer.enabled defaults to true")
	}
	url, calls := countingOptimizerLLM(t)
	llmCfg := defaults.LLM
	llmCfg.Endpoint, llmCfg.APIKey, llmCfg.Model = url, "k", "m"
	client := llm.New(&llmCfg, fnNoopLog)
	recs, tokens, rejected, err := defaultOptimizer(client).analyzeTable(
		context.Background(), sampleTableContext())
	if err != nil {
		t.Fatalf("analyzeTable: %v", err)
	}
	if calls.Load() != 1 || tokens != 120 {
		t.Fatalf("calls=%d tokens=%d, want 1/120", calls.Load(), tokens)
	}
	if len(recs) != 0 || rejected != 0 {
		t.Errorf("recs=%+v rejected=%d for an empty answer", recs, rejected)
	}
}

func TestOptimizerDefaultLLMBlockWithoutEndpointNeverCalls(t *testing.T) {
	url, calls := countingOptimizerLLM(t)
	client := llm.New(&config.DefaultConfig().LLM, fnNoopLog)
	if client.IsEnabled() {
		t.Fatal("default llm block must not be usable without endpoint/key")
	}
	_, _, _, err := defaultOptimizer(client).analyzeTable(
		context.Background(), sampleTableContext())
	if err == nil {
		t.Fatal("analyzeTable succeeded without an LLM")
	}
	if calls.Load() != 0 {
		t.Errorf("provider %s called %d times", url, calls.Load())
	}
}
