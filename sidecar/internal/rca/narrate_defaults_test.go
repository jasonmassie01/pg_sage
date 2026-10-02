package rca

import (
	"context"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// rca.narration_enabled now defaults to on. With no LLM configured the
// notification keeps the deterministic summary; with one configured the
// default config actually narrates through it.

func defaultRCAEngine() *Engine {
	rcaCfg := config.DefaultConfig().RCA
	return NewEngine(&rcaCfg, noopTestLog)
}

func TestNarrationDefaultOnWithoutLLMKeepsDeterministicSummary(t *testing.T) {
	inc := lockIncident(t)
	eng := defaultRCAEngine()
	if !eng.cfg.NarrationEnabled {
		t.Fatal("precondition: rca.narration_enabled defaults to true")
	}
	n := eng.narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "llm unavailable")
	if n.llmAttempted {
		t.Error("narration attempted an LLM call with no client")
	}
}

// The default llm config (enabled, no endpoint or key) yields a client
// that never reaches a provider.
func TestNarrationDefaultLLMConfigWithoutEndpointNeverCalls(t *testing.T) {
	inc := lockIncident(t)
	srv := newNarrServer(t, twoTurn(groundedFinal))
	eng := defaultRCAEngine()
	eng.WithLLM(llm.New(&config.DefaultConfig().LLM, noopTestLog))
	n := eng.narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "llm unavailable")
	if srv.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", srv.calls.Load())
	}
}

func TestNarrationDefaultOnCallsConfiguredLLM(t *testing.T) {
	inc := lockIncident(t)
	srv := newNarrServer(t, twoTurn(groundedFinal))
	llmCfg := config.DefaultConfig().LLM
	llmCfg.Endpoint, llmCfg.APIKey, llmCfg.Model = srv.srv.URL, "k", "m"
	eng := defaultRCAEngine()
	eng.WithLLM(llm.New(&llmCfg, noopTestLog))
	n := eng.narrate(context.Background(), inc)
	if n.Source != NarrationLLM {
		t.Fatalf("source = %q (fallback %q), want llm", n.Source, n.FallbackReason)
	}
	if srv.calls.Load() != 2 {
		t.Errorf("provider calls = %d, want 2 turns", srv.calls.Load())
	}
	if len(n.Citations) == 0 || n.Citations[0] != "E2" {
		t.Errorf("citations = %v, want E2", n.Citations)
	}
}

// Explicit narration_enabled=false wins even with a configured LLM.
func TestNarrationExplicitFalseNeverCallsConfiguredLLM(t *testing.T) {
	inc := lockIncident(t)
	srv := newNarrServer(t, func(int, w http.ResponseWriter) {
		writeCompletion(w, groundedFinal, nil)
	})
	eng := narrEngine(srv.srv.URL, false)
	n := eng.narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "narration disabled")
	if srv.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", srv.calls.Load())
	}
}
