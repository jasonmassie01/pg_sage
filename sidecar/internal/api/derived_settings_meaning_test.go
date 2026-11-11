package api

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// footprint-10: GET /api/v1/derived-settings explains in "meaning" what
// the rules read. It may name the collector's measured statement time
// only while a registered rule cites a collector-source measure, must
// name it then, and never claims "pg_sage's own cost" without one. No
// concurrent access tests: this reads a constant.

// rulesCiteCollector reports a registered rule citing a measure the
// collector's meter supplies once it has run.
func rulesCiteCollector(rules []selfconfig.Rule) bool {
	ev := selfconfig.Evidence{Source: selfconfig.SourceProbe,
		Relations: selfconfig.Known(10)}.WithCollectorCost(
		func() (selfconfig.CollectorCost, bool) {
			return selfconfig.CollectorCost{Cycles: 1, CycleMs: 10, SlowestMs: 5,
				SlowestStep: "tables", Timeouts: 1}, true
		})
	for _, r := range rules {
		if len(r.Cites) == 0 {
			continue // Cite() with no names lists every measure
		}
		for _, c := range ev.Cite(r.Cites...) {
			if c.Source == selfconfig.SourceCollector {
				return true
			}
		}
	}
	return false
}

func TestDerivedSettingsMeaning_CitesOnlyEvidenceTheRulesRead(t *testing.T) {
	cited := rulesCiteCollector(selfconfig.Rules())
	lower := strings.ToLower(derivedSettingsMeaning)
	if strings.Contains(lower, "own cost") && !cited {
		t.Fatalf("meaning claims pg_sage's own cost, but no rule cites a collector "+
			"measure: %q", derivedSettingsMeaning)
	}
	names := strings.Contains(lower, "collector's measured statement time")
	if names != cited {
		t.Fatalf("meaning names the collector's measured statement time = %v, a rule "+
			"cites a collector measure = %v: %q", names, cited, derivedSettingsMeaning)
	}
	for _, want := range []string{"catalog size", "scan times", "shadow", "restart"} {
		if !strings.Contains(derivedSettingsMeaning, want) {
			t.Errorf("meaning omits %q: %q", want, derivedSettingsMeaning)
		}
	}
}

func TestDerivedSettingsMeaning_NoRulesCiteNoCollector(t *testing.T) {
	if rulesCiteCollector(nil) {
		t.Fatal("no rules cite a collector measure")
	}
	if rulesCiteCollector([]selfconfig.Rule{{Key: "x"}}) {
		t.Fatal("a rule that cites nothing cites a collector measure")
	}
	if rulesCiteCollector([]selfconfig.Rule{{Key: "x", Cites: []string{"relations"}}}) {
		t.Fatal("a rule citing only the relation count cites a collector measure")
	}
}
