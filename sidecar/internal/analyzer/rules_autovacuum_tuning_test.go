package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func avTuneTable(schema, name string, live, upd, del int64) collector.TableStats {
	return collector.TableStats{
		SchemaName: schema, RelName: name,
		NLiveTup: live, NTupUpd: upd, NTupDel: del,
	}
}

// TestRuleAutovacuumTuning_HappyPath: a large, write-heavy table yields a
// reversible per-table reloption recommendation.
func TestRuleAutovacuumTuning_HappyPath(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AutovacuumTuneMinRows = 1_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		avTuneTable("public", "orders", 12_000_000, 10_000_000, 3_000_000),
	}}

	findings := ruleAutovacuumTuning(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "autovacuum_tuning" || f.ActionRisk != "safe" {
		t.Errorf("category/risk = %s/%s", f.Category, f.ActionRisk)
	}
	// 12M rows -> 0.02 tier.
	if !strings.Contains(f.RecommendedSQL, "autovacuum_vacuum_scale_factor = 0.02") {
		t.Errorf("RecommendedSQL = %q", f.RecommendedSQL)
	}
	if !strings.Contains(f.RecommendedSQL, `"public"."orders"`) {
		t.Errorf("identifier not quoted: %q", f.RecommendedSQL)
	}
	if !strings.Contains(f.RollbackSQL, "RESET (autovacuum_vacuum_scale_factor)") {
		t.Errorf("RollbackSQL = %q", f.RollbackSQL)
	}
}

// TestRuleAutovacuumTuning_BelowMinRows: small tables are skipped.
func TestRuleAutovacuumTuning_BelowMinRows(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AutovacuumTuneMinRows = 1_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		avTuneTable("public", "small", 500_000, 5_000_000, 0),
	}}
	if got := ruleAutovacuumTuning(snap, nil, cfg, nil); len(got) != 0 {
		t.Errorf("expected 0 findings for small table, got %d", len(got))
	}
}

// TestRuleAutovacuumTuning_LowChurn: a big but low-churn table is skipped.
func TestRuleAutovacuumTuning_LowChurn(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.AutovacuumTuneMinRows = 1_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		avTuneTable("public", "archive", 20_000_000, 100, 0), // churn << live
	}}
	if got := ruleAutovacuumTuning(snap, nil, cfg, nil); len(got) != 0 {
		t.Errorf("expected 0 findings for low-churn table, got %d", len(got))
	}
}

// TestRuleAutovacuumTuning_NilSnapshot: no panic, no findings.
func TestRuleAutovacuumTuning_NilSnapshot(t *testing.T) {
	cfg := testConfig()
	if got := ruleAutovacuumTuning(nil, nil, cfg, nil); got != nil {
		t.Errorf("expected nil for nil snapshot, got %v", got)
	}
}

// TestTargetScaleFactor_Tiers covers the size tiers.
func TestTargetScaleFactor_Tiers(t *testing.T) {
	cases := []struct {
		rows int64
		want float64
	}{
		{1_000_000, 0.05},
		{9_999_999, 0.05},
		{10_000_000, 0.02},
		{49_999_999, 0.02},
		{50_000_000, 0.01},
		{500_000_000, 0.01},
	}
	for _, c := range cases {
		if got := targetScaleFactor(c.rows); got != c.want {
			t.Errorf("targetScaleFactor(%d) = %v, want %v", c.rows, got, c.want)
		}
	}
}
