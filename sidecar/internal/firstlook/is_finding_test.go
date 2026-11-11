package firstlook

import (
	"testing"
)

// No concurrent access tests: IsFinding and CountItems are pure functions
// of their input.

func boolPtr(b bool) *bool { return &b }

// setupOnlyItems is what nearly every fresh install reports: no HypoPG and
// no auto_explain (info), no pg_stat_statements (a warning) and hidden
// query text (a warning).
func setupOnlyItems() []Item {
	_, items := ExtensionCapabilities(Extensions{Role: "sage_agent",
		StatStatementsLoaded: boolPtr(false), QueryTextVisible: boolPtr(false)},
		"self-managed")
	return items
}

func TestIsFindingWarningCatalogItem(t *testing.T) {
	it := Item{Rule: RuleDuplicateIndex, Severity: SeverityWarning, Object: "public.t_b"}
	if !it.IsFinding() {
		t.Fatalf("%+v is not a finding, want a warning duplicate index to be one", it)
	}
	crit := Item{Rule: RuleSequenceRunway, Severity: SeverityCritical, Object: "public.s"}
	if !crit.IsFinding() {
		t.Fatalf("%+v is not a finding, want a critical catalog item to be one", crit)
	}
}

// Invalid input for the metric: the setup notes of a fresh install are not
// findings at any severity. Fails before firstlook-07 (IsFinding did not
// exist and every item counted).
func TestIsFindingExcludesSetupNotes(t *testing.T) {
	items := setupOnlyItems()
	rules := map[string]int{}
	warnings := 0
	for _, it := range items {
		rules[it.Rule]++
		if it.Severity == SeverityWarning {
			warnings++
		}
		if it.IsFinding() {
			t.Errorf("setup note %s/%s (%s) counts as a finding", it.Rule, it.Object,
				it.Severity)
		}
	}
	if rules[RuleMissingExtension] != 3 || rules[RuleQueryTextHidden] != 1 || warnings != 2 {
		t.Fatalf("fixture items = %+v, want 3 missing extensions and hidden query text, "+
			"two of them warnings", items)
	}
	for _, sev := range []string{SeverityInfo, SeverityWarning, SeverityCritical} {
		for _, rule := range []string{RuleMissingExtension, RuleQueryTextHidden, RuleLogRCA} {
			if (Item{Rule: rule, Severity: sev}).IsFinding() {
				t.Errorf("%s at %s counts as a finding", rule, sev)
			}
		}
	}
}

func TestIsFindingBoundaries(t *testing.T) {
	cases := []struct {
		name string
		it   Item
		want bool
	}{
		{"info catalog item", Item{Rule: RuleRedundantIndex, Severity: SeverityInfo}, false},
		{"warning catalog item", Item{Rule: RuleRedundantIndex, Severity: SeverityWarning},
			true},
		{"critical posture item", Item{Rule: "AP-01", Section: SectionAgentPosture,
			Severity: SeverityCritical}, false},
		{"warning posture item", Item{Rule: "AP-02", Section: SectionAgentPosture,
			Severity: SeverityWarning}, false},
		{"unknown severity", Item{Rule: RuleDuplicateIndex, Severity: "urgent"}, false},
		{"empty severity", Item{Rule: RuleDuplicateIndex}, false},
		{"unknown section", Item{Rule: RuleDuplicateIndex, Section: "later",
			Severity: SeverityCritical}, false},
		{"zero item", Item{}, false},
	}
	for _, c := range cases {
		if got := c.it.IsFinding(); got != c.want {
			t.Errorf("%s: IsFinding(%+v) = %v, want %v", c.name, c.it, got, c.want)
		}
	}
}

func TestCountItemsMixedReport(t *testing.T) {
	items := append(setupOnlyItems(),
		Item{Rule: RuleDuplicateIndex, Severity: SeverityWarning},
		Item{Rule: RuleUnindexedFK, Severity: SeverityWarning},
		Item{Rule: RuleSequenceRunway, Severity: SeverityCritical},
		Item{Rule: RuleRedundantIndex, Severity: SeverityInfo},
		Item{Rule: RuleTestSchema, Severity: SeverityInfo},
		Item{Rule: RuleLogRCA, Severity: SeverityInfo},
		Item{Rule: "AP-01", Section: SectionAgentPosture, Severity: SeverityCritical})
	got := CountItems(items)
	want := ItemCounts{Findings: 3, Critical: 1, Setup: 5, Posture: 1, Notes: 2}
	if got != want {
		t.Fatalf("CountItems = %+v, want %+v", got, want)
	}
	if got.Total() != len(items) {
		t.Fatalf("Total = %d, want every one of the %d items", got.Total(), len(items))
	}
}

func TestCountItemsEmpty(t *testing.T) {
	for _, items := range [][]Item{nil, {}} {
		if got := CountItems(items); got != (ItemCounts{}) || got.Total() != 0 {
			t.Fatalf("CountItems(%v) = %+v, want zero counts", items, got)
		}
	}
}

func TestCountItemsSetupOnly(t *testing.T) {
	got := CountItems(setupOnlyItems())
	if got != (ItemCounts{Setup: 4}) {
		t.Fatalf("CountItems(setup only) = %+v, want 4 setup notes and no findings", got)
	}
}

func TestSummaryOfCountsItems(t *testing.T) {
	r := summarySample()
	r.Items = append(r.Items, setupOnlyItems()...)
	got := SummaryOf(r).Counts
	if got != (ItemCounts{Findings: 2, Critical: 1, Setup: 4}) {
		t.Fatalf("SummaryOf counts = %+v", got)
	}
	if n := SummaryOf(r).Items; n != len(r.Items) {
		t.Fatalf("SummaryOf items = %d, want every item (%d)", n, len(r.Items))
	}
}
