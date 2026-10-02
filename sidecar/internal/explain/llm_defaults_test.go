package explain

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// explain.enabled defaults to on. With only the default llm block (no
// endpoint or key) the endpoint keeps its deterministic plan summary and
// logs nothing; with a provider it narrates through it.

func deterministicResult() *ExplainResult {
	return &ExplainResult{
		Query:    "SELECT 1",
		PlanJSON: []byte(`[{"Plan":{"Node Type":"Result"}}]`),
		Summary:  "deterministic summary",
	}
}

func TestExplainDefaultLLMBlockKeepsDeterministicSummary(t *testing.T) {
	defaults := config.DefaultConfig()
	if !defaults.Explain.Enabled {
		t.Fatal("precondition: explain.enabled defaults to true")
	}
	var logged atomic.Int32
	logFn := func(string, string, ...any) { logged.Add(1) }
	ex := NewWithLLM(nil, &defaults.Explain, llm.New(&defaults.LLM, logFn), logFn)
	result := deterministicResult()
	ex.enhanceWithLLM(context.Background(), result)
	if result.Summary != "deterministic summary" || len(result.SlowBecause) != 0 {
		t.Fatalf("result changed without an LLM: %+v", result)
	}
	if logged.Load() != 0 {
		t.Errorf("unconfigured LLM logged %d lines", logged.Load())
	}
}

func TestExplainDefaultsCallConfiguredLLM(t *testing.T) {
	var calls atomic.Int32
	content := `{"summary":"llm summary","slow_because":["seq scan"],` +
		`"recommendations":["add index"]}`
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":30}}`, content)
		}))
	defer srv.Close()
	defaults := config.DefaultConfig()
	defaults.LLM.Endpoint, defaults.LLM.APIKey = srv.URL, "k"
	ex := NewWithLLM(nil, &defaults.Explain, llm.New(&defaults.LLM, noopLogFn),
		noopLogFn)
	result := deterministicResult()
	ex.enhanceWithLLM(context.Background(), result)
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	if result.Summary != "llm summary" {
		t.Errorf("summary = %q, want the LLM summary", result.Summary)
	}
}
