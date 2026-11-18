package lint

import "testing"

// review-01a: RuleIDs is the registry the review rubric validates its lint
// checks against: every built-in rule, once, in scan order. No concurrent
// access tests: it builds a fresh slice per call.
func TestRuleIDs_ListsEveryBuiltInRuleOnce(t *testing.T) {
	ids := RuleIDs()
	rules := defaultRules()
	if len(ids) != len(rules) || len(ids) < 16 {
		t.Fatalf("RuleIDs = %v, want the %d built-in rules", ids, len(rules))
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if id != rules[i].ID() || seen[id] {
			t.Fatalf("RuleIDs[%d] = %q, want %q once", i, id, rules[i].ID())
		}
		seen[id] = true
	}
	for _, want := range []string{"lint_int_pk", "lint_fk_type_mismatch",
		"lint_no_primary_key", "lint_timestamp_no_tz", "lint_serial_usage"} {
		if !seen[want] {
			t.Errorf("RuleIDs lacks %s", want)
		}
	}
	ids[0] = "mutated"
	if RuleIDs()[0] == "mutated" {
		t.Fatal("RuleIDs shares its slice with callers")
	}
}
