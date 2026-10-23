package impact

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// CHECK-IMP-16: any critical gives risky, any warning gives review, only
// info gives safe. The findings are built with the engine's own
// constructor, so a constructor that loses or rewrites a severity fails
// here. Stateless: no concurrent-access test applies.
func TestVerdictTableOf14RuleCombinations(t *testing.T) {
	type hit struct{ rule, sev string }
	const (
		I = changereview.SeverityInfo
		W = changereview.SeverityWarning
		C = changereview.SeverityCritical
	)
	cases := []struct {
		name string
		hits []hit
		mode string
		want string
	}{
		{"nothing", nil, changereview.ParseAST, changereview.VerdictSafe},
		{"info only", []hit{{RuleLockQueue, I}}, changereview.ParseAST,
			changereview.VerdictSafe},
		{"several infos", []hit{{RuleIndexBenefit, I}, {RuleNewObject, I},
			{RulePublished, I}}, changereview.ParseAST, changereview.VerdictSafe},
		{"one warning", []hit{{RuleIndexRedundant, W}}, changereview.ParseAST,
			changereview.VerdictReview},
		{"warning among infos", []hit{{RuleLockQueue, I}, {RuleFKUnindexed, W},
			{RuleNewObject, I}}, changereview.ParseAST, changereview.VerdictReview},
		{"one critical", []hit{{RuleColumnReferenced, C}}, changereview.ParseAST,
			changereview.VerdictRisky},
		{"critical after warnings", []hit{{RuleFKTypeMismatch, W}, {RuleBackfill, W},
			{RuleWillFail, C}}, changereview.ParseAST, changereview.VerdictRisky},
		{"critical before info", []hit{{RuleIndexUsed, C}, {RuleIndexBenefit, I}},
			changereview.ParseAST, changereview.VerdictRisky},
		{"unknown impact warns", []hit{{RuleImpactUnknown, W}}, changereview.ParseAST,
			changereview.VerdictReview},
		{"partitioned warns", []hit{{RulePartitioned, W}, {RuleLockQueue, I}},
			changereview.ParseAST, changereview.VerdictReview},
		{"lock queue critical", []hit{{RuleLockQueue, C}}, changereview.ParseAST,
			changereview.VerdictRisky},
		{"text mode alone", nil, changereview.ParseText, changereview.VerdictReview},
		{"text mode info", []hit{{RuleLockAndRewrite, I}}, changereview.ParseText,
			changereview.VerdictReview},
		{"text mode critical", []hit{{changereview.RuleTextMode, W},
			{RuleLockAndRewrite, C}}, changereview.ParseText, changereview.VerdictRisky},
	}
	if len(cases) != 14 {
		t.Fatalf("the verdict table has %d combinations, want 14", len(cases))
	}
	for _, c := range cases {
		var fs []changereview.Finding
		for _, h := range c.hits {
			f := newFinding(h.rule, h.sev, "title", "text", "stmt:1")
			if f.Severity != h.sev || f.RuleID != h.rule ||
				f.Origin != changereview.OriginRule || f.Kind != ruleNames[h.rule] {
				t.Fatalf("%s: newFinding(%s, %s) = %+v", c.name, h.rule, h.sev, f)
			}
			fs = append(fs, f)
		}
		if got := changereview.VerdictOf(fs, c.mode); got != c.want {
			t.Errorf("%s: verdict %s, want %s", c.name, got, c.want)
		}
	}
}

func TestEveryRuleHasAName(t *testing.T) {
	want := map[string]string{
		RuleLockAndRewrite: "lock_and_rewrite", RuleLockQueue: "lock_queue_exposure",
		RuleColumnReferenced: "column_still_referenced",
		RuleRelationReferenced: "relation_still_referenced",
		RuleWillFail:           "change_will_fail", RuleTypeChange: "type_change_impact",
		RuleIndexUsed: "index_still_used", RuleIndexRedundant: "index_redundant",
		RuleIndexBenefit: "index_benefit", RuleFKUnindexed: "fk_unindexed",
		RuleFKTypeMismatch: "fk_type_mismatch", RuleBackfill: "backfill_size",
		RulePublished: "published_table", RuleStatementPlan: "statement_plan",
		RuleNewObject: "new_object_antipattern", RulePartitioned: "partitioned_or_inherited",
		RuleImpactUnknown: "impact_unknown", changereview.RuleTextMode: "unparsed_or_text_mode",
	}
	for id, name := range want {
		if ruleNames[id] != name {
			t.Errorf("rule %s is named %q, want %q", id, ruleNames[id], name)
		}
	}
	if len(ruleNames) != len(want) {
		t.Errorf("%d rule names, want %d (CR-01..CR-18)", len(ruleNames), len(want))
	}
}
