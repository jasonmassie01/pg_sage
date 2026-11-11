package api

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// No concurrent access tests: the handler reads a fake whose state no
// request changes.

func countsFake(items []firstlook.Item) *fakeOnboardingReader {
	f := newFakeOnboarding()
	db := f.dbs["app"]
	report := *db.FirstLook
	report.Items = items
	db.FirstLook = &report
	f.dbs["app"] = db
	return f
}

func firstLookCounts(t *testing.T, items []firstlook.Item) (map[string]any, string) {
	t.Helper()
	_, body := obGet(t, onboardingHandler(countsFake(items)), "/api/v1/onboarding")
	db := body["databases"].([]any)[0].(map[string]any)
	detail, _ := onboardingStep(t, db, onboarding.StepFirstLook)["detail"].(string)
	return db["first_look"].(map[string]any), detail
}

// The summary separates findings from setup and posture notes; items keeps
// counting every item.
func TestOnboardingFirstLookSummaryCountsKinds(t *testing.T) {
	fl, detail := firstLookCounts(t, []firstlook.Item{
		{Rule: firstlook.RuleDuplicateIndex, Severity: firstlook.SeverityWarning},
		{Rule: firstlook.RuleSequenceRunway, Severity: firstlook.SeverityCritical},
		{Rule: firstlook.RuleRedundantIndex, Severity: firstlook.SeverityInfo},
		{Rule: firstlook.RuleMissingExtension, Severity: firstlook.SeverityWarning},
		{Rule: firstlook.RuleQueryTextHidden, Severity: firstlook.SeverityWarning},
		{Rule: "AP-01", Section: firstlook.SectionAgentPosture,
			Severity: firstlook.SeverityCritical},
	})
	want := map[string]float64{"items": 6, "findings": 2, "critical": 1, "setup": 2,
		"posture": 1, "notes": 1}
	for key, n := range want {
		if fl[key] != n {
			t.Errorf("first_look.%s = %v, want %v (%v)", key, fl[key], n, fl)
		}
	}
	if !strings.HasPrefix(detail, "2 findings (1 critical), 1 informational item, "+
		"2 setup notes, 1 posture note from the catalog.") {
		t.Fatalf("detail = %q", detail)
	}
}

// Invalid input for the old wording: a report of setup notes only reads
// "No problems found". Fails before firstlook-07 ("3 findings").
func TestOnboardingSetupOnlyFirstLookIsNoProblems(t *testing.T) {
	fl, detail := firstLookCounts(t, []firstlook.Item{
		{Rule: firstlook.RuleMissingExtension, Severity: firstlook.SeverityInfo},
		{Rule: firstlook.RuleMissingExtension, Severity: firstlook.SeverityWarning},
		{Rule: firstlook.RuleQueryTextHidden, Severity: firstlook.SeverityWarning},
	})
	if fl["items"] != float64(3) || fl["findings"] != float64(0) ||
		fl["setup"] != float64(3) {
		t.Fatalf("first look = %v, want 3 items, 0 findings, 3 setup notes", fl)
	}
	if !strings.HasPrefix(detail, "No problems found in the catalog.") ||
		!strings.Contains(detail, "3 setup notes") {
		t.Fatalf("detail = %q", detail)
	}
}

// Nil/empty: no items gives zero counts, all present in the JSON.
func TestOnboardingNoItemsZeroCounts(t *testing.T) {
	fl, _ := firstLookCounts(t, nil)
	for _, key := range []string{"items", "findings", "critical", "setup", "posture",
		"notes"} {
		if v, ok := fl[key]; !ok || v != float64(0) {
			t.Errorf("first_look.%s = %v (present %v), want 0", key, v, ok)
		}
	}
}
