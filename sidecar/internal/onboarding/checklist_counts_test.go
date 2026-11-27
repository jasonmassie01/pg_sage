package onboarding

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// No concurrent access tests: the first look step is a pure function of
// its input.

func firstLookDetailOf(c firstlook.ItemCounts) string {
	return step(Checklist(ChecklistInput{FirstLookReady: true, FirstLookChecks: 9,
		FirstLookCounts: c}), StepFirstLook).Detail
}

func TestFirstLookStepNamesEachKindOfItem(t *testing.T) {
	got := firstLookDetailOf(firstlook.ItemCounts{Findings: 3, Critical: 1, Setup: 2,
		Posture: 1})
	if want := "3 findings (1 critical), 2 setup notes, 1 posture note from the " +
		"catalog."; got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
	got = firstLookDetailOf(firstlook.ItemCounts{Findings: 1, Notes: 2, Posture: 3})
	if want := "1 finding, 2 informational items, 3 posture notes from the " +
		"catalog."; got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
}

// Invalid input for the old wording: setup notes alone are not findings.
// Fails before firstlook-07, which printed "4 findings from the catalog."
func TestFirstLookStepSetupNotesAreNotFindings(t *testing.T) {
	got := firstLookDetailOf(firstlook.ItemCounts{Setup: 4})
	if strings.Contains(got, "finding from") || strings.Contains(got, "findings from") {
		t.Fatalf("detail %q counts setup notes as findings", got)
	}
	if !strings.HasPrefix(got, "No problems found in the catalog.") ||
		!strings.Contains(got, "4 setup notes") {
		t.Fatalf("detail %q, want no problems found and the 4 setup notes named", got)
	}
}

func TestFirstLookStepEmptyCounts(t *testing.T) {
	got := firstLookDetailOf(firstlook.ItemCounts{})
	if want := "No problems found in the catalog. Query findings follow as statistics " +
		"accumulate."; got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
}

func TestFirstLookStepCountBoundaries(t *testing.T) {
	cases := []struct {
		c    firstlook.ItemCounts
		want string
	}{
		{firstlook.ItemCounts{Findings: 1}, "1 finding from the catalog."},
		{firstlook.ItemCounts{Findings: 2, Critical: 2}, "2 findings (2 critical) from " +
			"the catalog."},
		{firstlook.ItemCounts{Findings: 1, Setup: 1, Notes: 1, Posture: 1},
			"1 finding, 1 informational item, 1 setup note, 1 posture note from the " +
				"catalog."},
	}
	for _, c := range cases {
		if got := firstLookDetailOf(c.c); got != c.want {
			t.Errorf("counts %+v: detail = %q, want %q", c.c, got, c.want)
		}
	}
	// A critical posture note is not a finding: no problems in the catalog.
	got := firstLookDetailOf(firstlook.ItemCounts{Posture: 1})
	if !strings.HasPrefix(got, "No problems found in the catalog.") ||
		!strings.Contains(got, "1 posture note") {
		t.Fatalf("posture only: detail %q", got)
	}
}

// A partial first look with only setup notes scopes "no problems" to the
// checks that ran and still names the notes.
func TestFirstLookStepDegradedSetupOnly(t *testing.T) {
	s := step(Checklist(ChecklistInput{FirstLookReady: true, FirstLookChecks: 9,
		FirstLookDegraded: 2, FirstLookCounts: firstlook.ItemCounts{Setup: 1}}),
		StepFirstLook)
	for _, want := range []string{"2 of 9 checks could not run",
		"No problems found in the 7 checks that ran.", "1 setup note"} {
		if !strings.Contains(s.Detail, want) {
			t.Fatalf("detail %q lacks %q", s.Detail, want)
		}
	}
}
