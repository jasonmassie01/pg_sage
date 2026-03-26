package advisor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

func newTestAdvisor(enabled, llmEnabled bool, intervalSec int) *Advisor {
	cfg := &config.Config{}
	cfg.Advisor.Enabled = enabled
	cfg.Advisor.IntervalSeconds = intervalSec
	cfg.LLM.Enabled = llmEnabled
	return New(nil, cfg, nil, nil, func(string, string, ...any) {})
}

func TestAdvisor_ShouldRun_FirstRun(t *testing.T) {
	adv := newTestAdvisor(true, true, 86400)
	// lastRunAt is zero-value; time.Since(zero) is huge, so ShouldRun returns true.
	if !adv.ShouldRun() {
		t.Fatal("expected ShouldRun() == true on first run (zero lastRunAt)")
	}
}

func TestAdvisor_ShouldRun_RecentRun(t *testing.T) {
	adv := newTestAdvisor(true, true, 86400)
	adv.lastRunAt = time.Now()
	if adv.ShouldRun() {
		t.Fatal("expected ShouldRun() == false immediately after setting lastRunAt to now")
	}
}

func TestAdvisor_ShouldRun_StaleRun(t *testing.T) {
	adv := newTestAdvisor(true, true, 86400)
	adv.lastRunAt = time.Now().Add(-25 * time.Hour)
	if !adv.ShouldRun() {
		t.Fatal("expected ShouldRun() == true when lastRunAt is 25h ago (interval=24h)")
	}
}

func TestAdvisor_LatestFindings_Empty(t *testing.T) {
	adv := newTestAdvisor(true, true, 86400)
	findings := adv.LatestFindings()
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(findings))
	}
}

func TestAdvisor_LatestFindings_ReturnsCopy(t *testing.T) {
	adv := newTestAdvisor(true, true, 86400)

	// Directly assign findings to the advisor.
	adv.mu.Lock()
	adv.findings = []analyzer.Finding{
		{Category: "test", Severity: "info", Title: "original"},
	}
	adv.mu.Unlock()

	// Get a copy and mutate it.
	got := adv.LatestFindings()
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	got[0].Title = "mutated"

	// Verify the internal slice is unchanged.
	adv.mu.Lock()
	defer adv.mu.Unlock()
	if adv.findings[0].Title != "original" {
		t.Fatal("LatestFindings did not return a copy; internal state was mutated")
	}
}

func TestAdvisor_Analyze_Disabled(t *testing.T) {
	adv := newTestAdvisor(false, true, 86400)
	findings, err := adv.Analyze(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatalf("expected nil findings when advisor disabled, got %v", findings)
	}
}

func TestAdvisor_Analyze_LLMDisabled(t *testing.T) {
	adv := newTestAdvisor(true, false, 86400)
	findings, err := adv.Analyze(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatalf("expected nil findings when LLM disabled, got %v", findings)
	}
}

func TestAdvisor_ShouldRun_TransitionsAfterManualSet(t *testing.T) {
	adv := newTestAdvisor(true, true, 86400)

	// First call: lastRunAt is zero, so ShouldRun is true.
	if !adv.ShouldRun() {
		t.Fatal("expected ShouldRun() == true on first call")
	}

	// Simulate what Analyze does: set lastRunAt to now.
	adv.mu.Lock()
	adv.lastRunAt = time.Now()
	adv.mu.Unlock()

	// Now ShouldRun should be false.
	if adv.ShouldRun() {
		t.Fatal("expected ShouldRun() == false after setting lastRunAt to now")
	}
}
