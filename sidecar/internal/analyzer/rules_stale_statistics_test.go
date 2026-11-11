package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestRuleStaleStatistics_NeverAnalyzed(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "events", NLiveTup: 50000, NTupIns: 50000,
			ModSinceAnalyze: 50000, AnalyzeThreshold: 50},
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
			NTupUpd: 20000, ModSinceAnalyze: 20000, AnalyzeThreshold: 5050,
			LastAutoanalyze: &old},
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
			NTupUpd: 20000, ModSinceAnalyze: 20000, AnalyzeThreshold: 5050,
			LastAnalyze: &recent},
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
		{SchemaName: "public", RelName: "small", NLiveTup: 100, NTupIns: 100},  // below minRows
		{SchemaName: "public", RelName: "static", NLiveTup: 50000, NTupIns: 0}, // no writes
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

// rules-10: staleness is judged by the rows modified since the last
// analyze against the autoanalyze threshold, not by lifetime counters.

// No concurrent access tests: ruleStaleStatisticsAt is a pure function of
// its snapshot, config and clock.

func rules10Config() *config.Config {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	return cfg
}

var rules10Now = time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)

func staleTable(
	name string, mods int64, threshold float64, last *time.Time,
) collector.TableStats {
	return collector.TableStats{SchemaName: "public", RelName: name, Relkind: "r",
		NLiveTup: 50000, NTupIns: 50000, NTupUpd: 20000,
		ModSinceAnalyze: mods, AnalyzeThreshold: threshold, LastAnalyze: last}
}

func staleIdents(findings []Finding) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range findings {
		if f.Category == "stale_statistics" {
			out[f.ObjectIdentifier] = f
		}
	}
	return out
}

// A lookup table loaded once and analyzed: its lifetime counters are
// large, but nothing changed since the analyze. Today's code re-flags it
// once the analyze is older than the cutoff.
func TestRuleStaleStatistics_StaticTableWithLifetimeWritesNotFlagged(t *testing.T) {
	old := rules10Now.AddDate(0, 0, -30)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("lookup", 0, 5050, &old)}}
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Fatalf("static table flagged: %+v", got)
	}
}

// CHECK-08: four simulated weeks of the same static table give no finding.
func TestRuleStaleStatistics_StaticTableNoWeeklyReflag(t *testing.T) {
	analyzed := rules10Now.AddDate(0, 0, -1)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("lookup", 0, 5050, &analyzed)}}
	for week := 1; week <= 4; week++ {
		at := rules10Now.AddDate(0, 0, 7*week)
		if got := ruleStaleStatisticsAt(snap, rules10Config(), at); len(got) != 0 {
			t.Fatalf("week %d re-flagged the static table: %+v", week, got)
		}
	}
}

func TestRuleStaleStatistics_ThresholdBoundary(t *testing.T) {
	old := rules10Now.AddDate(0, 0, -30)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("at_threshold", 5050, 5050, &old),
		staleTable("past_threshold", 5051, 5050, &old)}}
	got := staleIdents(ruleStaleStatisticsAt(snap, rules10Config(), rules10Now))
	if _, ok := got["public.at_threshold"]; ok {
		t.Errorf("mod_since_analyze == threshold was flagged")
	}
	f, ok := got["public.past_threshold"]
	if !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.past_threshold only", got)
	}
	if f.Detail["mod_since_analyze"] != int64(5051) ||
		f.Detail["analyze_threshold"] != float64(5050) {
		t.Errorf("detail = %v, want mod_since_analyze 5051 and analyze_threshold 5050",
			f.Detail)
	}
	if f.Detail["never_analyzed"] != false || f.RecommendedSQL !=
		`ANALYZE "public"."past_threshold";` || f.Severity != "warning" {
		t.Errorf("finding = %+v", f)
	}
}

// The cutoff still applies: a heavily modified table analyzed yesterday
// is left to autoanalyze.
func TestRuleStaleStatistics_ModifiedButRecentlyAnalyzed(t *testing.T) {
	recent := rules10Now.AddDate(0, 0, -1)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("busy", 40000, 5050, &recent)}}
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Fatalf("recently analyzed table flagged: %+v", got)
	}
}

// The cutoff boundary: analyzed exactly staleDays ago is not stale yet,
// one second earlier is.
func TestRuleStaleStatistics_CutoffBoundary(t *testing.T) {
	atCutoff := rules10Now.AddDate(0, 0, -7)
	pastCutoff := atCutoff.Add(-time.Second)
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("at_cutoff", 9000, 5050, &atCutoff),
		staleTable("past_cutoff", 9000, 5050, &pastCutoff)}}
	got := staleIdents(ruleStaleStatisticsAt(snap, rules10Config(), rules10Now))
	if _, ok := got["public.past_cutoff"]; !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.past_cutoff only", got)
	}
}

// A never-analyzed table is flagged once rows changed; with no change
// since the (reset) statistics, it is not: a stats reset clears both
// last_analyze and mod_since_analyze.
func TestRuleStaleStatistics_NeverAnalyzedNeedsModifications(t *testing.T) {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("fresh", 50000, 50, nil),
		staleTable("after_reset", 0, 5050, nil)}}
	got := staleIdents(ruleStaleStatisticsAt(snap, rules10Config(), rules10Now))
	f, ok := got["public.fresh"]
	if !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.fresh only", got)
	}
	if f.Detail["never_analyzed"] != true || f.Detail["mod_since_analyze"] != int64(50000) {
		t.Errorf("detail = %v", f.Detail)
	}
}

// Invalid input: a zero-value table row (a legacy snapshot without the
// rules-10 columns) is never flagged.
func TestRuleStaleStatistics_LegacyRowWithoutModColumnsNotFlagged(t *testing.T) {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "legacy", NLiveTup: 90000, NTupIns: 90000}}}
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Fatalf("legacy row flagged: %+v", got)
	}
}

// Defaults: unset analyze_stale_days and analyze_stale_min_rows fall back
// to 7 days and 10000 rows.
func TestRuleStaleStatistics_DefaultsWithoutConfig(t *testing.T) {
	cfg := testConfig()
	sixDays, eightDays := rules10Now.AddDate(0, 0, -6), rules10Now.AddDate(0, 0, -8)
	small := staleTable("small", 9000, 50, &eightDays)
	small.NLiveTup = 9999
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		staleTable("six", 9000, 5050, &sixDays),
		staleTable("eight", 9000, 5050, &eightDays), small}}
	got := staleIdents(ruleStaleStatisticsAt(snap, cfg, rules10Now))
	if _, ok := got["public.eight"]; !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want public.eight only", got)
	}
}
