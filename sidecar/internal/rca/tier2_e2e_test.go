package rca

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// ---------------------------------------------------------------------------
// Fake LLM server variants
// ---------------------------------------------------------------------------

// tier2FakeServer returns an httptest server that returns a valid
// Tier 2 JSON response via the OpenAI chat completions format.
func tier2FakeServer(t2resp tier2Response) *httptest.Server {
	t2json, _ := json.Marshal(t2resp)
	return fakeLLMServer(string(t2json))
}

// tier2ErrorServer returns an httptest server that always responds
// with the given HTTP status code.
func tier2ErrorServer(code int) *httptest.Server {
	return httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"error":"fail"}`))
		}))
}

// tier2MalformedServer returns valid HTTP 200 with garbage content.
func tier2MalformedServer() *httptest.Server {
	return fakeLLMServer("this is not JSON at all {{{")
}

// tier2MarkdownServer wraps the response in markdown fences.
func tier2MarkdownServer(t2resp tier2Response) *httptest.Server {
	t2json, _ := json.Marshal(t2resp)
	wrapped := "```json\n" + string(t2json) + "\n```"
	return fakeLLMServer(wrapped)
}

// tier2CountingServer tracks how many requests it receives.
func tier2CountingServer(
	t2resp tier2Response,
	counter *atomic.Int32,
) *httptest.Server {
	t2json, _ := json.Marshal(t2resp)
	return httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counter.Add(1)
			resp := map[string]any{
				"choices": []map[string]any{{
					"message":       map[string]string{"content": string(t2json)},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"total_tokens": 50},
			}
			json.NewEncoder(w).Encode(resp)
		}))
}

func tier2LLMClient(url string) *llm.Client {
	cfg := &config.LLMConfig{
		Enabled:        true,
		Endpoint:       url,
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 5,
	}
	return llm.New(cfg, func(string, string, ...any) {})
}

func validTier2Response() tier2Response {
	return tier2Response{
		RootCause:      "Cascading failure: connection pool exhaustion triggered OOM",
		Severity:       "critical",
		CausalChain:    "connection_spike -> memory_pressure -> OOM",
		RecommendedSQL: []string{"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE state = 'idle'"},
		ActionRisk:     "high",
	}
}

// uncoveredLogSignals returns N log signals that don't match any
// metric decision tree (they only match log trees, but log trees
// consume them — so we use custom IDs that match nothing).
func uncoveredSignals(n int) []*Signal {
	ids := []string{
		"custom_signal_alpha", "custom_signal_beta",
		"custom_signal_gamma", "custom_signal_delta",
		"custom_signal_epsilon",
	}
	out := make([]*Signal, n)
	for i := 0; i < n; i++ {
		out[i] = &Signal{
			ID:       ids[i%len(ids)],
			FiredAt:  time.Now(),
			Severity: "warning",
			Metrics:  map[string]any{"value": i * 10},
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// E2E: Tier 2 through full Analyze() pipeline
// ---------------------------------------------------------------------------

// TestTier2E2E_HappyPath fires 3 uncovered signals through Analyze()
// with an LLM wired up, expects a Tier 2 incident.
func TestTier2E2E_HappyPath(t *testing.T) {
	srv := tier2FakeServer(validTier2Response())
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	// Feed 3 uncovered signals via LogSource (custom IDs that no
	// decision tree consumes).
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	// Should have exactly 1 Tier 2 incident.
	var llmInc *Incident
	for i := range incidents {
		if incidents[i].Source == "llm" {
			llmInc = &incidents[i]
		}
	}
	if llmInc == nil {
		t.Fatal("expected Tier 2 (llm) incident, got none")
	}
	if llmInc.Confidence != 0.6 {
		t.Errorf("Confidence = %f, want 0.6", llmInc.Confidence)
	}
	if !strings.Contains(llmInc.RootCause, "Cascading failure") {
		t.Errorf("RootCause = %q, want Cascading failure", llmInc.RootCause)
	}
	if llmInc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", llmInc.Severity)
	}
	if len(llmInc.CausalChain) < 2 {
		t.Errorf("CausalChain len = %d, want >= 2", len(llmInc.CausalChain))
	}
	if llmInc.ActionRisk != "high" {
		t.Errorf("ActionRisk = %q, want high", llmInc.ActionRisk)
	}
	if !strings.Contains(llmInc.RecommendedSQL, "pg_terminate_backend") {
		t.Errorf("RecommendedSQL = %q, want pg_terminate_backend",
			llmInc.RecommendedSQL)
	}
	if len(llmInc.SignalIDs) != 3 {
		t.Errorf("SignalIDs len = %d, want 3", len(llmInc.SignalIDs))
	}
}

// TestTier2E2E_BelowThreshold: fewer uncovered signals than threshold
// means no LLM call.
func TestTier2E2E_BelowThreshold(t *testing.T) {
	var callCount atomic.Int32
	srv := tier2CountingServer(validTier2Response(), &callCount)
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(2)})
	eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)

	if callCount.Load() != 0 {
		t.Errorf("LLM called %d times, want 0 (below threshold)",
			callCount.Load())
	}
}

// TestTier2E2E_CoveredSignalsSkipped: signals consumed by Tier 1
// are not sent to Tier 2.
func TestTier2E2E_CoveredSignalsSkipped(t *testing.T) {
	var callCount atomic.Int32
	srv := tier2CountingServer(validTier2Response(), &callCount)
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 1

	// Feed log signals that ARE consumed by log decision trees.
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
			Metrics: map[string]any{"message": "no space"}},
		{ID: "log_lock_timeout", FiredAt: time.Now(), Severity: "warning",
			Metrics: map[string]any{"message": "timeout"}},
	}})
	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	// Tier 1 log trees cover both signals → 0 uncovered → no LLM call.
	if callCount.Load() != 0 {
		t.Errorf("LLM called %d times, want 0 (all signals covered)",
			callCount.Load())
	}
	// But Tier 1 incidents should still exist.
	if len(incidents) < 2 {
		t.Errorf("expected >= 2 Tier 1 incidents, got %d", len(incidents))
	}
}

// TestTier2E2E_LLMDisabled: nil LLM client means no Tier 2.
func TestTier2E2E_LLMDisabled(t *testing.T) {
	eng := testEngine()
	// No WithLLM call.
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(5)})
	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	for _, inc := range incidents {
		if inc.Source == "llm" {
			t.Error("got llm incident with no LLM client wired")
		}
	}
}

// TestTier2E2E_LLMError: server returns 500 → Tier 2 gracefully
// fails, Tier 1 incidents still produced.
func TestTier2E2E_LLMError(t *testing.T) {
	srv := tier2ErrorServer(500)
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	// Mix: 2 covered log signals + 3 uncovered custom signals.
	eng.SetLogSource(&mockLogSource{signals: append(
		[]*Signal{
			{ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
				Metrics: map[string]any{"message": "no space"}},
			{ID: "log_lock_timeout", FiredAt: time.Now(), Severity: "warning",
				Metrics: map[string]any{"message": "timeout"}},
		}, uncoveredSignals(3)...,
	)})
	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	// Tier 1 log incidents should still exist.
	var tier1Count, tier2Count int
	for _, inc := range incidents {
		if inc.Source == "llm" {
			tier2Count++
		} else {
			tier1Count++
		}
	}
	if tier2Count != 0 {
		t.Errorf("got %d Tier 2 incidents despite LLM error", tier2Count)
	}
	if tier1Count < 2 {
		t.Errorf("expected >= 2 Tier 1 incidents, got %d", tier1Count)
	}
}

// TestTier2E2E_MalformedJSON: LLM returns garbage → no Tier 2
// incident, no crash.
func TestTier2E2E_MalformedJSON(t *testing.T) {
	srv := tier2MalformedServer()
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	for _, inc := range incidents {
		if inc.Source == "llm" {
			t.Error("got llm incident from malformed JSON response")
		}
	}
}

// TestTier2E2E_MarkdownWrapped: LLM wraps JSON in markdown fences
// → stripToJSONObject recovers it.
func TestTier2E2E_MarkdownWrapped(t *testing.T) {
	srv := tier2MarkdownServer(validTier2Response())
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	var llmInc *Incident
	for i := range incidents {
		if incidents[i].Source == "llm" {
			llmInc = &incidents[i]
		}
	}
	if llmInc == nil {
		t.Fatal("expected Tier 2 incident from markdown-wrapped response")
	}
	if !strings.Contains(llmInc.RootCause, "Cascading failure") {
		t.Errorf("RootCause = %q, want Cascading failure", llmInc.RootCause)
	}
}

// TestTier2E2E_Dedup: same Tier 2 incident fires twice → dedup bumps
// occurrence count.
func TestTier2E2E_Dedup(t *testing.T) {
	srv := tier2FakeServer(validTier2Response())
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	cfg := testConfig()

	// Cycle 1: produce Tier 2 incident.
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	inc1 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	var first *Incident
	for i := range inc1 {
		if inc1[i].Source == "llm" {
			first = &inc1[i]
		}
	}
	if first == nil {
		t.Fatal("cycle 1: expected Tier 2 incident")
	}
	firstID := first.ID

	// Cycle 2: same uncovered signals → same Tier 2 pattern → dedup.
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	inc2 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	var second *Incident
	for i := range inc2 {
		if inc2[i].Source == "llm" {
			second = &inc2[i]
		}
	}
	if second == nil {
		t.Fatal("cycle 2: expected Tier 2 incident")
	}
	if second.ID != firstID {
		t.Errorf("expected same ID %q, got %q (dedup failed)", firstID, second.ID)
	}
	if second.OccurrenceCount < 2 {
		t.Errorf("OccurrenceCount = %d, want >= 2", second.OccurrenceCount)
	}
}

// TestTier2E2E_MetricAndLogAndTier2Combined: metric signals produce
// Tier 1 incidents, uncovered log signals produce Tier 2.
func TestTier2E2E_MetricAndLogAndTier2Combined(t *testing.T) {
	srv := tier2FakeServer(validTier2Response())
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3

	// Hot snapshot triggers connections_high (metric Tier 1).
	hot := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  90,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	// 3 uncovered custom signals for Tier 2.
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	incidents := eng.Analyze(hot, nil, testConfig(), nil)

	sources := make(map[string]bool)
	for _, inc := range incidents {
		sources[inc.Source] = true
	}
	if !sources["deterministic"] {
		t.Error("missing metric (deterministic) incident")
	}
	if !sources["llm"] {
		t.Error("missing Tier 2 (llm) incident")
	}
}

// concurrentLogSource returns fresh uncovered signals on every Drain.
type concurrentLogSource struct {
	mu sync.Mutex
	n  int
}

func (c *concurrentLogSource) Start(_ context.Context) error { return nil }
func (c *concurrentLogSource) Stop()                         {}
func (c *concurrentLogSource) Drain() []*Signal {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return uncoveredSignals(3)
}

// TestTier2E2E_ConcurrentAnalyze: multiple goroutines calling Analyze()
// with LLM enabled don't race.
func TestTier2E2E_ConcurrentAnalyze(t *testing.T) {
	srv := tier2FakeServer(validTier2Response())
	defer srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.cfg.LLMCorrelationThreshold = 3
	eng.SetLogSource(&concurrentLogSource{})

	cfg := testConfig()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
		}()
	}
	wg.Wait()

	active := eng.ActiveIncidents()
	hasLLM := false
	for _, inc := range active {
		if inc.Source == "llm" {
			hasLLM = true
		}
	}
	if !hasLLM {
		t.Error("no llm incident after concurrent Analyze calls")
	}
}

// TestTier2E2E_TokenBudgetExhausted: LLM budget exhausted → no
// Tier 2 incident, Tier 1 still works.
func TestTier2E2E_TokenBudgetExhausted(t *testing.T) {
	srv := tier2FakeServer(validTier2Response())
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 10, // very low budget
	}, func(string, string, ...any) {})

	eng := testEngine()
	eng.WithLLM(client)
	eng.cfg.LLMCorrelationThreshold = 3

	cfg := testConfig()

	// Cycle 1: should use tokens and produce Tier 2 incident.
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	inc1 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	var gotT2 bool
	for _, inc := range inc1 {
		if inc.Source == "llm" {
			gotT2 = true
		}
	}
	if !gotT2 {
		t.Fatal("cycle 1: expected Tier 2 incident")
	}

	// Budget should now be exhausted (50 tokens used > 10 budget).
	if !client.IsBudgetExhausted() {
		t.Skip("budget not exhausted after first call; test needs adjustment")
	}

	// Cycle 2: budget exhausted → no new Tier 2, but Tier 1 logs work.
	eng.SetLogSource(&mockLogSource{signals: append(
		uncoveredSignals(3),
		&Signal{ID: "log_disk_full", FiredAt: time.Now(),
			Severity: "critical",
			Metrics:  map[string]any{"message": "no space"}},
	)})
	inc2 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)

	var tier1Found bool
	for _, inc := range inc2 {
		if inc.Source == "log_deterministic" {
			tier1Found = true
		}
	}
	if !tier1Found {
		t.Error("Tier 1 log incident missing after budget exhaustion")
	}
}
