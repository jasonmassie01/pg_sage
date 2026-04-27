package rca

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func noopLog(_, _ string, _ ...any) {}

// fakeLLMServer returns an httptest server with a valid chat response.
func fakeLLMServer(content string) *httptest.Server {
	return httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := map[string]any{
				"choices": []map[string]any{{
					"message":       map[string]string{"content": content},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"total_tokens": 50},
			}
			json.NewEncoder(w).Encode(resp)
		}))
}

// newLLMClient creates an llm.Client pointing at the given test server.
func newLLMClient(url string) *llm.Client {
	cfg := &config.LLMConfig{
		Enabled:        true,
		Endpoint:       url,
		APIKey:         "test-key",
		Model:          "test",
		TimeoutSeconds: 5,
	}
	return llm.New(cfg, noopLog)
}

// testSignals returns N distinct signals for test use.
func testSignals(n int) []*Signal {
	sigs := make([]*Signal, n)
	for i := range sigs {
		sigs[i] = &Signal{
			ID:       "sig_" + string(rune('a'+i)),
			FiredAt:  time.Now(),
			Severity: "warning",
			Metrics:  map[string]any{"val": i},
		}
	}
	return sigs
}

// --- findUncoveredSignals ---

func TestFindUncoveredSignals_AllCovered(t *testing.T) {
	signals := testSignals(3)
	incidents := []Incident{{
		SignalIDs: []string{"sig_a", "sig_b", "sig_c"},
	}}
	got := findUncoveredSignals(signals, incidents)
	if len(got) != 0 {
		t.Errorf("expected 0 uncovered, got %d", len(got))
	}
}

func TestFindUncoveredSignals_SomeUncovered(t *testing.T) {
	signals := testSignals(3)
	incidents := []Incident{{SignalIDs: []string{"sig_a"}}}
	got := findUncoveredSignals(signals, incidents)
	if len(got) != 2 {
		t.Fatalf("expected 2 uncovered, got %d", len(got))
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids["sig_b"] || !ids["sig_c"] {
		t.Errorf("unexpected IDs: %v", ids)
	}
}

func TestFindUncoveredSignals_NoIncidents(t *testing.T) {
	signals := testSignals(4)
	got := findUncoveredSignals(signals, nil)
	if len(got) != 4 {
		t.Errorf("expected 4 uncovered, got %d", len(got))
	}
}

func TestFindUncoveredSignals_EmptySignals(t *testing.T) {
	incidents := []Incident{{SignalIDs: []string{"x"}}}
	got := findUncoveredSignals(nil, incidents)
	if len(got) != 0 {
		t.Errorf("expected 0 uncovered, got %d", len(got))
	}
}

func TestFindUncoveredSignals_DuplicateIDInIncidents(t *testing.T) {
	signals := testSignals(2)
	incidents := []Incident{
		{SignalIDs: []string{"sig_a"}},
		{SignalIDs: []string{"sig_a", "sig_b"}},
	}
	got := findUncoveredSignals(signals, incidents)
	if len(got) != 0 {
		t.Errorf("expected 0 uncovered, got %d", len(got))
	}
}

// --- runTier2Correlation ---

func TestRunTier2Correlation_NilLLMClient(t *testing.T) {
	eng := testEngine()
	// llmClient is nil by default
	got := eng.runTier2Correlation(testSignals(5), nil)
	if got != nil {
		t.Errorf("expected nil for nil llmClient, got %v", got)
	}
}

func TestRunTier2Correlation_LLMDisabled(t *testing.T) {
	eng := testEngine()
	cfg := &config.LLMConfig{Enabled: false, Endpoint: "x", APIKey: "k"}
	eng.WithLLM(llm.New(cfg, noopLog))
	got := eng.runTier2Correlation(testSignals(5), nil)
	if got != nil {
		t.Errorf("expected nil for disabled LLM, got %v", got)
	}
}

func TestRunTier2Correlation_BelowThreshold(t *testing.T) {
	srv := fakeLLMServer(`{"root_cause":"test"}`)
	defer srv.Close()

	eng := testEngine()
	eng.cfg.LLMCorrelationThreshold = 3
	eng.WithLLM(newLLMClient(srv.URL))

	// Only 2 uncovered signals, threshold is 3 → nil
	got := eng.runTier2Correlation(testSignals(2), nil)
	if got != nil {
		t.Errorf("expected nil when uncovered < threshold, got %v", got)
	}
}

func TestRunTier2Correlation_DefaultThreshold(t *testing.T) {
	srv := fakeLLMServer(`{"root_cause":"test"}`)
	defer srv.Close()

	eng := testEngine()
	eng.cfg.LLMCorrelationThreshold = 0 // should default to 3
	eng.WithLLM(newLLMClient(srv.URL))

	// 2 signals < default threshold 3 → nil
	got := eng.runTier2Correlation(testSignals(2), nil)
	if got != nil {
		t.Errorf("expected nil when under default threshold, got %v", got)
	}
}

func TestRunTier2Correlation_AtThreshold(t *testing.T) {
	resp := `{
		"root_cause": "shared buffer exhaustion",
		"severity": "warning",
		"causal_chain": "A -> B -> C",
		"recommended_sql": ["SELECT 1"],
		"action_risk": "low"
	}`
	srv := fakeLLMServer(resp)
	defer srv.Close()

	eng := testEngine()
	eng.cfg.LLMCorrelationThreshold = 3
	eng.WithLLM(newLLMClient(srv.URL))

	got := eng.runTier2Correlation(testSignals(3), nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(got))
	}
	if got[0].RootCause != "shared buffer exhaustion" {
		t.Errorf("RootCause = %q", got[0].RootCause)
	}
	if got[0].Source != "llm" {
		t.Errorf("Source = %q, want llm", got[0].Source)
	}
}

// --- buildTier2SystemPrompt ---

func TestBuildTier2SystemPrompt(t *testing.T) {
	p := buildTier2SystemPrompt()
	if p == "" {
		t.Fatal("system prompt is empty")
	}
	for _, kw := range []string{"PostgreSQL", "root cause", "JSON"} {
		if !strings.Contains(p, kw) {
			t.Errorf("system prompt missing keyword %q", kw)
		}
	}
}

// --- buildTier2UserPrompt ---

func TestBuildTier2UserPrompt_Format(t *testing.T) {
	sigs := []*Signal{
		{
			ID: "sig_x", FiredAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Severity: "critical",
			Metrics:  map[string]any{"cpu": 99.5},
		},
		{
			ID: "sig_y", FiredAt: time.Date(2025, 1, 1, 0, 1, 0, 0, time.UTC),
			Severity: "warning",
			Metrics:  map[string]any{"count": 42},
		},
	}
	got := buildTier2UserPrompt(sigs)
	if !strings.Contains(got, "2 uncovered signals") {
		t.Errorf("missing signal count header: %s", got)
	}
	if !strings.Contains(got, "1. ID=sig_x") {
		t.Errorf("missing signal 1 numbering: %s", got)
	}
	if !strings.Contains(got, "2. ID=sig_y") {
		t.Errorf("missing signal 2 numbering: %s", got)
	}
	if !strings.Contains(got, "severity=critical") {
		t.Errorf("missing severity: %s", got)
	}
	if !strings.Contains(got, `"cpu":99.5`) {
		t.Errorf("missing metrics JSON: %s", got)
	}
}

func TestBuildTier2UserPrompt_Empty(t *testing.T) {
	got := buildTier2UserPrompt(nil)
	if !strings.Contains(got, "0 uncovered signals") {
		t.Errorf("expected 0 count header, got: %s", got)
	}
}

// --- parseTier2Response ---

func TestParseTier2Response_ValidJSON(t *testing.T) {
	raw := `{
		"root_cause": "lock contention on pg_catalog",
		"severity": "critical",
		"causal_chain": "heavy writes -> WAL flush -> lock wait",
		"recommended_sql": ["VACUUM FULL pg_catalog.pg_class"],
		"action_risk": "high"
	}`
	uncovered := testSignals(3)
	inc, err := parseTier2Response(raw, uncovered)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inc.RootCause != "lock contention on pg_catalog" {
		t.Errorf("RootCause = %q", inc.RootCause)
	}
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", inc.Severity)
	}
	if inc.ActionRisk != "high" {
		t.Errorf("ActionRisk = %q, want high", inc.ActionRisk)
	}
	if len(inc.SignalIDs) != 3 {
		t.Errorf("SignalIDs len = %d, want 3", len(inc.SignalIDs))
	}
}

func TestParseTier2Response_MarkdownWrapped(t *testing.T) {
	raw := "```json\n" +
		`{"root_cause":"bloat","severity":"warning",` +
		`"causal_chain":"A -> B","recommended_sql":[],` +
		`"action_risk":"low"}` +
		"\n```"
	inc, err := parseTier2Response(raw, testSignals(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inc.RootCause != "bloat" {
		t.Errorf("RootCause = %q, want bloat", inc.RootCause)
	}
}

func TestParseTier2Response_EmptyRootCause(t *testing.T) {
	raw := `{"root_cause":"","severity":"warning",` +
		`"causal_chain":"","recommended_sql":[],"action_risk":"low"}`
	_, err := parseTier2Response(raw, testSignals(1))
	if err == nil {
		t.Fatal("expected error for empty root_cause")
	}
	if !strings.Contains(err.Error(), "empty root_cause") {
		t.Errorf("error = %q, want 'empty root_cause'", err)
	}
}

func TestParseTier2Response_InvalidJSON(t *testing.T) {
	_, err := parseTier2Response("not json at all", testSignals(1))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseTier2Response_MissingOptionalFields(t *testing.T) {
	// Only root_cause is required; other fields can be zero-valued.
	raw := `{"root_cause":"minimal"}`
	inc, err := parseTier2Response(raw, testSignals(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inc.RootCause != "minimal" {
		t.Errorf("RootCause = %q", inc.RootCause)
	}
	// Missing severity defaults to "warning" via buildTier2Incident
	if inc.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", inc.Severity)
	}
	// Missing action_risk defaults to "medium"
	if inc.ActionRisk != "medium" {
		t.Errorf("ActionRisk = %q, want medium", inc.ActionRisk)
	}
}

// --- buildTier2Incident ---

func TestBuildTier2Incident(t *testing.T) {
	t.Run("source is always llm", func(t *testing.T) {
		inc := buildTier2Incident(
			tier2Response{RootCause: "t", Severity: "warning"},
			testSignals(1))
		if inc.Source != "llm" {
			t.Errorf("Source = %q, want llm", inc.Source)
		}
	})
	t.Run("confidence is 0.6", func(t *testing.T) {
		inc := buildTier2Incident(
			tier2Response{RootCause: "t"}, testSignals(1))
		if inc.Confidence != 0.6 {
			t.Errorf("Confidence = %f, want 0.6", inc.Confidence)
		}
	})
	t.Run("invalid severity normalizes to warning", func(t *testing.T) {
		inc := buildTier2Incident(
			tier2Response{RootCause: "t", Severity: "bogus"},
			testSignals(1))
		if inc.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", inc.Severity)
		}
	})
	t.Run("invalid risk normalizes to medium", func(t *testing.T) {
		inc := buildTier2Incident(
			tier2Response{RootCause: "t", ActionRisk: "extreme"},
			testSignals(1))
		if inc.ActionRisk != "medium" {
			t.Errorf("ActionRisk = %q, want medium", inc.ActionRisk)
		}
	})
	t.Run("valid severity and risk preserved", func(t *testing.T) {
		inc := buildTier2Incident(tier2Response{
			RootCause: "t", Severity: "critical", ActionRisk: "high",
		}, testSignals(1))
		if inc.Severity != "critical" {
			t.Errorf("Severity = %q", inc.Severity)
		}
		if inc.ActionRisk != "high" {
			t.Errorf("ActionRisk = %q", inc.ActionRisk)
		}
	})
	t.Run("signal IDs collected", func(t *testing.T) {
		sigs := testSignals(3)
		inc := buildTier2Incident(
			tier2Response{RootCause: "t"}, sigs)
		if len(inc.SignalIDs) != 3 {
			t.Fatalf("len = %d, want 3", len(inc.SignalIDs))
		}
		for i, s := range sigs {
			if inc.SignalIDs[i] != s.ID {
				t.Errorf("[%d] = %q, want %q", i, inc.SignalIDs[i], s.ID)
			}
		}
	})
	t.Run("recommended SQL joined", func(t *testing.T) {
		inc := buildTier2Incident(tier2Response{
			RootCause:      "t",
			RecommendedSQL: []string{"SELECT 1", "VACUUM"},
		}, testSignals(1))
		if inc.RecommendedSQL != "SELECT 1; VACUUM" {
			t.Errorf("RecommendedSQL = %q", inc.RecommendedSQL)
		}
	})
	t.Run("nil SQL yields empty string", func(t *testing.T) {
		inc := buildTier2Incident(
			tier2Response{RootCause: "t"}, testSignals(1))
		if inc.RecommendedSQL != "" {
			t.Errorf("RecommendedSQL = %q, want empty", inc.RecommendedSQL)
		}
	})
}

// --- parseCausalChainString ---

func TestParseCausalChainString_ASCII(t *testing.T) {
	sigs := testSignals(3)
	chain := parseCausalChainString("A -> B -> C", sigs)
	if len(chain) != 3 {
		t.Fatalf("chain len = %d, want 3", len(chain))
	}
	for i, want := range []string{"A", "B", "C"} {
		if chain[i].Description != want {
			t.Errorf("chain[%d].Description = %q, want %q",
				i, chain[i].Description, want)
		}
		if chain[i].Order != i+1 {
			t.Errorf("chain[%d].Order = %d, want %d",
				i, chain[i].Order, i+1)
		}
	}
}

func TestParseCausalChainString_Unicode(t *testing.T) {
	sigs := testSignals(2)
	chain := parseCausalChainString("A \u2192 B", sigs)
	if len(chain) != 2 {
		t.Fatalf("chain len = %d, want 2", len(chain))
	}
	if chain[0].Description != "A" || chain[1].Description != "B" {
		t.Errorf("descriptions = [%q, %q]",
			chain[0].Description, chain[1].Description)
	}
}

func TestParseCausalChainString_Empty(t *testing.T) {
	chain := parseCausalChainString("", nil)
	if len(chain) != 0 {
		t.Errorf("chain len = %d, want 0 for empty input", len(chain))
	}
}

func TestParseCausalChainString_SingleItem(t *testing.T) {
	sigs := testSignals(1)
	chain := parseCausalChainString("only node", sigs)
	if len(chain) != 1 {
		t.Fatalf("chain len = %d, want 1", len(chain))
	}
	if chain[0].Description != "only node" {
		t.Errorf("Description = %q", chain[0].Description)
	}
	if chain[0].Signal != "sig_a" {
		t.Errorf("Signal = %q, want sig_a", chain[0].Signal)
	}
}

func TestParseCausalChainString_MorePartsThanSignals(t *testing.T) {
	sigs := testSignals(1) // only 1 signal but 3 chain parts
	chain := parseCausalChainString("A -> B -> C", sigs)
	if len(chain) != 3 {
		t.Fatalf("chain len = %d, want 3", len(chain))
	}
	// First link gets signal ID, rest get empty string
	if chain[0].Signal != "sig_a" {
		t.Errorf("chain[0].Signal = %q, want sig_a", chain[0].Signal)
	}
	if chain[1].Signal != "" {
		t.Errorf("chain[1].Signal = %q, want empty", chain[1].Signal)
	}
}

// --- stripToJSONObject ---

func TestStripToJSONObject(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"clean JSON", `{"key": "value"}`, `{"key": "value"}`},
		{"markdown fenced", "```json\n{\"k\":\"v\"}\n```", `{"k":"v"}`},
		{"nested braces", `{"o":{"i":1}}`, `{"o":{"i":1}}`},
		{"leading text", `Result: {"rc":"t"}`, `{"rc":"t"}`},
		{"whitespace only", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripToJSONObject(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("no braces strips fences", func(t *testing.T) {
		got := stripToJSONObject("```json\nno braces\n```")
		if strings.Contains(got, "```") {
			t.Errorf("fences not stripped: %q", got)
		}
		if !strings.Contains(got, "no braces") {
			t.Errorf("content lost: %q", got)
		}
	})
}
