package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestRuleWraparoundFreeze_WarningAndCritical(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "warm", XIDAge: 160_000_000},
		{SchemaName: "public", RelName: "hot", XIDAge: 210_000_000},
		{SchemaName: "public", RelName: "fine", XIDAge: 1_000_000},
	}}
	f := ruleWraparoundFreeze(snap, nil, cfg, nil)
	if len(f) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(f))
	}
	bySeverity := map[string]Finding{}
	for _, x := range f {
		bySeverity[x.Severity] = x
	}
	if _, ok := bySeverity["warning"]; !ok {
		t.Error("expected a warning finding")
	}
	crit, ok := bySeverity["critical"]
	if !ok {
		t.Fatal("expected a critical finding for age >= 200M")
	}
	if !strings.Contains(crit.RecommendedSQL, `VACUUM (FREEZE) "public"."hot"`) {
		t.Errorf("RecommendedSQL = %q", crit.RecommendedSQL)
	}
	if crit.ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q", crit.ActionRisk)
	}
}

func TestRuleWraparoundFreeze_NilAndBelowThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	if got := ruleWraparoundFreeze(nil, nil, cfg, nil); got != nil {
		t.Errorf("expected nil for nil snapshot")
	}
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "young", XIDAge: 100},
	}}
	if got := ruleWraparoundFreeze(snap, nil, cfg, nil); len(got) != 0 {
		t.Errorf("expected 0 findings below threshold, got %d", len(got))
	}
}
