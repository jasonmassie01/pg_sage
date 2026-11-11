package onboarding

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// No concurrent access tests: firstLookStep is a pure function of its input.

func TestFirstLookStepWithDegradedChecksIsNotHealthy(t *testing.T) {
	s := step(Checklist(ChecklistInput{Connected: true, FirstLookReady: true,
		FirstLookChecks: 9, FirstLookDegraded: 2}), StepFirstLook)
	if strings.Contains(s.Detail, "No problems found in the catalog") {
		t.Fatalf("detail %q calls a partial first look healthy", s.Detail)
	}
	if !strings.Contains(s.Detail, "First look partial: 2 of 9 checks could not run") {
		t.Fatalf("detail %q does not name the 2 degraded checks of 9", s.Detail)
	}
	if !strings.Contains(s.Detail, "No problems found in the 7 checks that ran") {
		t.Fatalf("detail %q does not scope the clean result to the checks that ran", s.Detail)
	}
	if !s.Done {
		t.Fatalf("partial first look = %+v, want done (7 checks ran)", s)
	}
}

func TestFirstLookStepEveryCheckDegradedIsNotDone(t *testing.T) {
	s := step(Checklist(ChecklistInput{Connected: true, FirstLookReady: true,
		FirstLookChecks: 9, FirstLookDegraded: 9}), StepFirstLook)
	if s.Done {
		t.Fatalf("first look with every check degraded = %+v, want not done", s)
	}
	if strings.Contains(s.Detail, "No problems") || !strings.Contains(s.Detail, "9") {
		t.Fatalf("detail %q must not say no problems and must name the 9 checks", s.Detail)
	}
}

func TestFirstLookStepDegradedWithFindings(t *testing.T) {
	s := step(Checklist(ChecklistInput{Connected: true, FirstLookReady: true,
		FirstLookCounts: firstlook.ItemCounts{Findings: 3}, FirstLookChecks: 9,
		FirstLookDegraded: 1, TTFFSeconds: fp(4)}), StepFirstLook)
	for _, want := range []string{"1 of 9 checks could not run", "3 findings", "4 s"} {
		if !strings.Contains(s.Detail, want) {
			t.Fatalf("detail %q lacks %q", s.Detail, want)
		}
	}
	if !s.Done {
		t.Fatalf("step = %+v, want done", s)
	}
}

func TestFirstLookStepHealthyTextUnchanged(t *testing.T) {
	// Happy path: no degraded checks keeps today's text, with or without counts.
	for _, in := range []ChecklistInput{
		{FirstLookReady: true, FirstLookCounts: firstlook.ItemCounts{Findings: 3},
			FirstLookChecks: 9},
		{FirstLookReady: true, FirstLookCounts: firstlook.ItemCounts{Findings: 3}},
	} {
		s := step(Checklist(in), StepFirstLook)
		if !s.Done || s.Detail != "3 findings from the catalog." {
			t.Fatalf("input %+v: step = %+v", in, s)
		}
	}
	s := step(Checklist(ChecklistInput{FirstLookReady: true, FirstLookChecks: 9}),
		StepFirstLook)
	if !s.Done || !strings.HasPrefix(s.Detail, "No problems found in the catalog.") {
		t.Fatalf("clean first look = %+v", s)
	}
}

func TestFirstLookStepTruncatedChecksAreNamed(t *testing.T) {
	s := step(Checklist(ChecklistInput{FirstLookReady: true,
		FirstLookCounts: firstlook.ItemCounts{Findings: 50},
		FirstLookChecks: 9, FirstLookTruncated: 1}), StepFirstLook)
	if !strings.Contains(s.Detail, "50 findings") ||
		!strings.Contains(s.Detail, "1 check shows only its first findings") {
		t.Fatalf("detail %q does not say one check was truncated", s.Detail)
	}
}

func TestFirstLookStepBoundaries(t *testing.T) {
	// One check of one degraded is every check: not done.
	if s := step(Checklist(ChecklistInput{FirstLookReady: true, FirstLookChecks: 1,
		FirstLookDegraded: 1}), StepFirstLook); s.Done {
		t.Fatalf("1 of 1 degraded = %+v, want not done", s)
	}
	// All but one degraded: one check ran, so it is done but partial.
	s := step(Checklist(ChecklistInput{FirstLookReady: true, FirstLookChecks: 9,
		FirstLookDegraded: 8}), StepFirstLook)
	if !s.Done || !strings.Contains(s.Detail, "8 of 9") ||
		!strings.Contains(s.Detail, "the 1 check that ran") {
		t.Fatalf("8 of 9 degraded = %+v", s)
	}
}

func TestFirstLookStepInconsistentCountsFailClosed(t *testing.T) {
	// Invalid input: more degraded than counted checks (a report written by a
	// different version) is treated as every check degraded, never as healthy.
	s := step(Checklist(ChecklistInput{FirstLookReady: true, FirstLookDegraded: 2}),
		StepFirstLook)
	if s.Done || strings.Contains(s.Detail, "No problems") {
		t.Fatalf("degraded without a check count = %+v, want not done", s)
	}
	// Not ready ignores the counts.
	s = step(Checklist(ChecklistInput{FirstLookChecks: 9, FirstLookDegraded: 2}),
		StepFirstLook)
	if s.Done || !strings.Contains(s.Detail, "Reading the catalog") {
		t.Fatalf("not ready = %+v", s)
	}
}
