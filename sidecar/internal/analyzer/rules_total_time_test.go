package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestRuleTotalTimeHeavy_Critical(t *testing.T) {
	// 500ms mean * 1000 calls = 500000ms delta over 60s.
	// pct = 500000 / 60000 * 100 = 833% → critical.
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-60 * time.Second),
		Queries: []collector.QueryStats{
			{QueryID: 1, TotalExecTime: 0},
		},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		Queries: []collector.QueryStats{
			{
				QueryID:       1,
				Query:         "SELECT * FROM t",
				Calls:         1000,
				MeanExecTime:  500,
				TotalExecTime: 500000,
			},
		},
	}
	cfg := &config.Config{}

	findings := ruleTotalTimeHeavy(cur, prev, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf(
			"severity = %q, want critical",
			findings[0].Severity,
		)
	}
	pct, ok := findings[0].Detail["pct_wall_clock"].(float64)
	if !ok || pct < 800 {
		t.Errorf("pct_wall_clock = %v, want >800", pct)
	}
}

func TestRuleTotalTimeHeavy_BelowThreshold(t *testing.T) {
	// 1ms mean * 100 calls = 100ms delta over 60s.
	// threshold = 60*0.10*1000 = 6000ms; 100 < 6000 → skip.
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-60 * time.Second),
		Queries: []collector.QueryStats{
			{QueryID: 1, TotalExecTime: 0},
		},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		Queries: []collector.QueryStats{
			{
				QueryID:       1,
				Calls:         100,
				MeanExecTime:  1,
				TotalExecTime: 100,
			},
		},
	}
	cfg := &config.Config{}

	findings := ruleTotalTimeHeavy(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestRuleTotalTimeHeavy_NoPrevious(t *testing.T) {
	cur := &collector.Snapshot{
		CollectedAt: time.Now(),
		Queries: []collector.QueryStats{
			{QueryID: 1, TotalExecTime: 999999},
		},
	}
	cfg := &config.Config{}

	findings := ruleTotalTimeHeavy(cur, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestRuleTotalTimeHeavy_NewQuery(t *testing.T) {
	// Query only in current (not in previous) → skipped.
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-60 * time.Second),
		Queries:     []collector.QueryStats{},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		Queries: []collector.QueryStats{
			{QueryID: 99, TotalExecTime: 999999},
		},
	}
	cfg := &config.Config{}

	findings := ruleTotalTimeHeavy(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestRuleTotalTimeHeavy_NegativeDelta(t *testing.T) {
	// Stats reset: current total < previous → skipped.
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-60 * time.Second),
		Queries: []collector.QueryStats{
			{QueryID: 1, TotalExecTime: 999999},
		},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		Queries: []collector.QueryStats{
			{QueryID: 1, TotalExecTime: 100},
		},
	}
	cfg := &config.Config{}

	findings := ruleTotalTimeHeavy(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}
