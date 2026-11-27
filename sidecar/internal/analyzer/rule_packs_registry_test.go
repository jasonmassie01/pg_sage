package analyzer

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// No concurrent access tests: both lists are package-level and read-only.

// The rule-pack vocabulary (findingcat.AnalyzerRules) must name every
// snapshot rule that owns categories, with the same categories (market-11).
func TestAnalyzerRulesMatchAllRules(t *testing.T) {
	byName := map[string][]string{}
	for _, r := range findingcat.AnalyzerRules() {
		byName[r.Name] = r.Categories
	}
	for _, spec := range AllRules {
		if len(spec.Categories) == 0 {
			continue
		}
		got, ok := byName[spec.Name]
		if !ok || !slices.Equal(got, spec.Categories) {
			t.Errorf("rule %s: registry %v (%v), AllRules %v",
				spec.Name, got, ok, spec.Categories)
		}
		delete(byName, spec.Name)
	}
	if len(byName) != 0 {
		t.Errorf("registry rules not in AllRules: %v", byName)
	}
}
