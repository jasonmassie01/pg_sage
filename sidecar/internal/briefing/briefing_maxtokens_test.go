package briefing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-05: the briefing's completion cap is its own constant, not
// llm.context_budget_tokens (which is the trim target for attached
// context). Reasoning models get the client's reasoning reserve on top.

// capCapture records the completion cap of each chat request.
type capCapture struct {
	mu   sync.Mutex
	caps []int
}

func (c *capCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens           int `json:"max_tokens"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		c.mu.Lock()
		c.caps = append(c.caps, body.MaxTokens+body.MaxCompletionTokens)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"# Briefing"},` +
			`"finish_reason":"stop"}],"usage":{"total_tokens":7}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *capCapture) last(t *testing.T) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.caps) == 0 {
		t.Fatal("no chat request reached the server")
	}
	return c.caps[len(c.caps)-1]
}

func TestBriefingMaxTokens_IgnoresContextBudget(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  int
	}{
		{"test-model", 4096},
		{"gemini-2.5-flash", 4096 + 16384},
	} {
		for _, budget := range []int{0, 512, 4096, 8192, 65536} {
			var cc capCapture
			srv := cc.server(t)
			client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL,
				APIKey: "k", Model: tc.model, TimeoutSeconds: 5,
				TokenBudgetDaily: 1000000, ContextBudgetTokens: budget}, noopLog)
			w := &Worker{cfg: &config.Config{LLM: config.LLMConfig{
				ContextBudgetTokens: budget}}, llm: client, logFn: noopLog}
			if _, _, err := w.enhanceWithLLM(context.Background(), "data"); err != nil {
				t.Fatalf("%s budget %d: %v", tc.model, budget, err)
			}
			if got := cc.last(t); got != tc.want {
				t.Errorf("%s budget %d: wire cap = %d, want %d",
					tc.model, budget, got, tc.want)
			}
		}
	}
}

func TestBriefingMaxTokens_Constant(t *testing.T) {
	if briefingMaxTokens != 4096 {
		t.Fatalf("briefingMaxTokens = %d, want 4096", briefingMaxTokens)
	}
}
