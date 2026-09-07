package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestRuleStaleStatistics_NeverAnalyzed(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "events", NLiveTup: 50000, NTupIns: 50000},
	}}
	f := ruleStaleStatisticsAt(snap, cfg, now)
	if len(f) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(f))
	}
	if f[0].Category != "stale_statistics" || f[0].ActionRisk != "safe" {
		t.Errorf("category/risk = %s/%s", f[0].Category, f[0].ActionRisk)
	}
	if f[0].RecommendedSQL != `ANALYZE "public"."events";` {
		t.Errorf("RecommendedSQL = %q", f[0].RecommendedSQL)
	}
	if f[0].Detail["never_analyzed"] != true {
		t.Error("expected never_analyzed=true")
	}
}

func TestRuleStaleStatistics_Stale(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -30)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "orders", NLiveTup: 50000,
			NTupUpd: 20000, LastAutoanalyze: &old},
	}}
	if got := ruleStaleStatisticsAt(snap, cfg, now); len(got) != 1 {
		t.Fatalf("expected 1 stale finding, got %d", len(got))
	}
}

func TestRuleStaleStatistics_RecentlyAnalyzed(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	recent := now.AddDate(0, 0, -1)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "orders", NLiveTup: 50000,
			NTupUpd: 20000, LastAnalyze: &recent},
	}}
	if got := ruleStaleStatisticsAt(snap, cfg, now); len(got) != 0 {
		t.Errorf("expected 0 findings for recently-analyzed table, got %d", len(got))
	}
}

func TestRuleStaleStatistics_SkipsSmallAndStatic(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "small", NLiveTup: 100, NTupIns: 100},       // below minRows
		{SchemaName: "public", RelName: "static", NLiveTup: 50000, NTupIns: 0},      // no writes
	}}
	if got := ruleStaleStatisticsAt(snap, cfg, now); len(got) != 0 {
		t.Errorf("expected 0 findings, got %d", len(got))
	}
}

func TestRuleStaleStatistics_NilSnapshot(t *testing.T) {
	if got := ruleStaleStatisticsAt(nil, testConfig(), time.Now()); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}
