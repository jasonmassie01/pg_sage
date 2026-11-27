package analyzer

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/findingcat"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// No concurrent access tests: Categories reads the init-built registry.

func TestAllRulesCategoriesAreAnalyzerCategories(t *testing.T) {
	own := Categories()
	if len(own) < 30 {
		t.Fatalf("Categories() = %d entries, want the analyzer's rule categories", len(own))
	}
	for _, spec := range AllRules {
		for _, c := range spec.Categories {
			if !slices.Contains(own, c) {
				t.Errorf("rule %s owns %q, which the registry does not give the analyzer",
					spec.Name, c)
			}
		}
	}
	for _, c := range own {
		entry, ok := findingcat.Lookup(c)
		if !ok || entry.Subsystem != "rules" || entry.Producer != findingcat.ProducerAnalyzer {
			t.Errorf("analyzer category %q = %+v, want a rules entry", c, entry)
		}
	}
}

func TestSeparatelyCalledRuleCategoriesAreRegistered(t *testing.T) {
	// Rules the analyzer loop calls outside AllRules.
	for _, c := range []string{"xid_wraparound", "connection_leak", "query_regression",
		"lock_chain", "extension_drift", "plan_regression", "sort_without_index",
		"work_mem_promotion", "seq_scan_heavy", categorySageFootprint, categorySelfCost,
		categorySelfBudget, CategoryCloneSchemas, CategoryAppManagedIndex} {
		if !slices.Contains(Categories(), c) {
			t.Errorf("separately called rule category %q is not an analyzer category", c)
		}
	}
}

func TestTuningAndOptimizerCategoriesAreRegistered(t *testing.T) {
	for _, c := range TuningCategories() {
		if entry, ok := findingcat.Lookup(c); !ok || entry.Prefix {
			t.Errorf("tuning agent category %q has no registry entry", c)
		}
	}
	optimizerNames := findingcat.Names(findingcat.BySubsystem("optimizer"))
	for _, c := range optimizer.Categories() {
		if !slices.Contains(optimizerNames, c) {
			t.Errorf("optimizer category %q is not in the optimizer subsystem", c)
		}
	}
	if entry, _ := findingcat.Lookup(SequenceRunwayCategory); entry.Subsystem != "forecaster" {
		t.Errorf("%s = %+v, want the forecaster subsystem", SequenceRunwayCategory, entry)
	}
}
