package findingcat

import (
	"strings"
	"testing"
)

// No concurrent access tests: the rule list and protected set are built at
// init and only read.

func TestResolveDisabled_ExpandsRulesAndCategories(t *testing.T) {
	d, err := ResolveDisabled([]string{"inactive_slots", "slow_query"})
	if err != nil {
		t.Fatalf("ResolveDisabled: %v", err)
	}
	if !d.Rules["inactive_slots"] || len(d.Rules) != 1 {
		t.Fatalf("Rules = %v, want inactive_slots only", d.Rules)
	}
	for _, c := range []string{"inactive_slot", "slow_replication_slot", "slow_query"} {
		if !d.Categories[c] {
			t.Errorf("category %q not disabled: %v", c, d.Categories)
		}
	}
	if len(d.Categories) != 3 {
		t.Fatalf("Categories = %v, want exactly three", d.Categories)
	}
}

func TestResolveDisabled_RefusesProtectedAndUnknown(t *testing.T) {
	for name, want := range map[string]string{
		"wraparound_freeze": "wraparound", "xid_wraparound": "wraparound",
		"sequence_exhaustion": "sequence", "sage_self_budget": "pg_sage",
		"sage_anything_new": "pg_sage", "nope": "unknown rule or category",
		"schema_lint:": "unknown rule or category",
	} {
		_, err := ResolveDisabled([]string{"unused_index", name})
		if err == nil || !strings.Contains(err.Error(), want) ||
			!strings.Contains(err.Error(), name) {
			t.Errorf("ResolveDisabled(%q) = %v, want an error naming it and %q",
				name, err, want)
		}
	}
}

func TestResolveDisabled_EmptyIsNothing(t *testing.T) {
	d, err := ResolveDisabled(nil)
	if err != nil || len(d.Rules) != 0 || len(d.Categories) != 0 {
		t.Fatalf("ResolveDisabled(nil) = %+v, %v", d, err)
	}
	if d.Any() {
		t.Fatal("an empty set reports Any")
	}
}

func TestAnalyzerRules_CategoriesAreAnalyzerEntries(t *testing.T) {
	rules := AnalyzerRules()
	if len(rules) < 20 {
		t.Fatalf("AnalyzerRules = %d entries", len(rules))
	}
	for _, r := range rules {
		for _, c := range r.Categories {
			e, ok := Lookup(c)
			if !ok || e.Producer != ProducerAnalyzer || e.Prefix {
				t.Errorf("rule %s category %q = %+v, want an analyzer entry", r.Name, c, e)
			}
		}
	}
	if e, ok := Lookup("custom:no_pk"); !ok || e.Subsystem != "custom" {
		t.Fatalf("custom:<id> = %+v, %v; want the custom family", e, ok)
	}
}
