package rca

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// TestTier2Live_RealGemini hits a real Gemini API endpoint to validate
// that the Tier 2 LLM correlation produces a valid incident from
// uncovered signals. Skipped unless GEMINI_API_KEY is set.
//
// Run: GEMINI_API_KEY=your-key go test -count=1 -v -run TestTier2Live ./internal/rca/
func TestTier2Live_RealGemini(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live LLM test")
	}

	client := llm.New(&config.LLMConfig{
		Enabled:        true,
		Endpoint:       "https://generativelanguage.googleapis.com/v1beta/openai",
		APIKey:         apiKey,
		Model:          "gemini-2.5-flash",
		TimeoutSeconds: 30,
	}, func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	})

	eng := testEngine()
	eng.WithLLM(client)
	eng.cfg.LLMCorrelationThreshold = 3

	// Feed 3 uncovered signals that no decision tree handles.
	now := time.Now()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "custom_io_bottleneck", FiredAt: now, Severity: "warning",
			Metrics: map[string]any{
				"read_time_ms": 450, "write_time_ms": 320,
				"iops":         12000,
			}},
		{ID: "custom_memory_pressure", FiredAt: now, Severity: "warning",
			Metrics: map[string]any{
				"shared_buffers_pct": 98.5, "temp_files_count": 15,
				"work_mem_spills":    8,
			}},
		{ID: "custom_connection_churn", FiredAt: now, Severity: "warning",
			Metrics: map[string]any{
				"connections_per_sec": 45, "avg_session_ms": 120,
				"pool_exhaustion":    true,
			}},
	}})

	incidents := eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil)

	var llmInc *Incident
	for i := range incidents {
		if incidents[i].Source == "llm" {
			llmInc = &incidents[i]
		}
	}
	if llmInc == nil {
		t.Fatal("expected Tier 2 (llm) incident from real Gemini")
	}

	t.Logf("RootCause: %s", llmInc.RootCause)
	t.Logf("Severity: %s", llmInc.Severity)
	t.Logf("ActionRisk: %s", llmInc.ActionRisk)
	t.Logf("CausalChain: %d links", len(llmInc.CausalChain))
	t.Logf("RecommendedSQL: %s", llmInc.RecommendedSQL)

	if llmInc.RootCause == "" {
		t.Error("RootCause is empty")
	}
	if llmInc.Confidence != 0.6 {
		t.Errorf("Confidence = %f, want 0.6", llmInc.Confidence)
	}
	if llmInc.Source != "llm" {
		t.Errorf("Source = %q, want llm", llmInc.Source)
	}
	if llmInc.Severity != "warning" && llmInc.Severity != "critical" {
		t.Errorf("Severity = %q, want warning or critical",
			llmInc.Severity)
	}
	if len(llmInc.CausalChain) == 0 {
		t.Error("CausalChain is empty")
	}
	if len(llmInc.SignalIDs) != 3 {
		t.Errorf("SignalIDs = %v, want 3 entries", llmInc.SignalIDs)
	}

	// Validate the causal chain has meaningful descriptions.
	for _, link := range llmInc.CausalChain {
		if strings.TrimSpace(link.Description) == "" {
			t.Errorf("CausalChain[%d] has empty description", link.Order)
		}
	}
}
