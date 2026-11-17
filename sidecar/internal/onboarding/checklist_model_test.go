package onboarding

import (
	"strings"
	"testing"
	"time"
)

// ux-04b: the Model step sits between First look and MCP token and is done
// only when a model is configured and its last call answered. No
// concurrent access tests: Checklist and ModelStatusOf are pure functions.

func TestChecklistModelStepSitsBetweenFirstLookAndMCP(t *testing.T) {
	steps := Checklist(ChecklistInput{})
	want := []string{StepConnected, StepExtensions, StepFirstLook, StepModel,
		StepMCPToken, StepNotifications, StepGrantMore}
	if len(steps) != len(want) {
		t.Fatalf("steps = %+v, want %v", steps, want)
	}
	for i, id := range want {
		if steps[i].ID != id {
			t.Fatalf("step %d = %s, want %s", i, steps[i].ID, id)
		}
	}
	m := step(steps, StepModel)
	if m.Label != "Model" || m.Done || !m.Optional || m.Link != "#/settings" {
		t.Fatalf("model step = %+v, want optional, not done, label Model, link #/settings", m)
	}
}

func TestChecklistModelStepStates(t *testing.T) {
	for name, tc := range map[string]struct {
		model  *ModelStatus
		done   bool
		detail string
	}{
		"unknown": {nil, false, "unknown"},
		"not set": {&ModelStatus{}, false, "LLM features stay off until a model is set"},
		"bad config": {&ModelStatus{Configured: true, ConfigurationError: "auth must be x"},
			false, "auth must be x"},
		"never called":  {&ModelStatus{Configured: true}, false, "has not answered yet"},
		"last call 401": {&ModelStatus{Configured: true, LastErrorClass: "auth"}, false, "auth"},
		"answered":      {&ModelStatus{Configured: true, Answered: true}, true, "answered"},
	} {
		t.Run(name, func(t *testing.T) {
			s := step(Checklist(ChecklistInput{Model: tc.model}), StepModel)
			if s.Done != tc.done || !strings.Contains(s.Detail, tc.detail) {
				t.Fatalf("model step = %+v, want done %v and detail with %q", s, tc.done,
					tc.detail)
			}
		})
	}
}

// A model whose answer is newer than its last error has passed its last
// probe; an error after the success has not.
func TestModelStatusOfOrdersSuccessAndError(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	for name, tc := range map[string]struct {
		in   ModelClient
		want ModelStatus
	}{
		"disabled":      {ModelClient{}, ModelStatus{}},
		"enabled never": {ModelClient{Enabled: true}, ModelStatus{Configured: true}},
		"config error": {ModelClient{Enabled: true, ConfigurationError: "bad"},
			ModelStatus{Configured: true, ConfigurationError: "bad"}},
		"success only": {ModelClient{Enabled: true, LastSuccessAt: &t0},
			ModelStatus{Configured: true, Answered: true}},
		"error after success": {ModelClient{Enabled: true, LastSuccessAt: &t0,
			LastErrorAt: &t1, LastErrorClass: "rate"},
			ModelStatus{Configured: true, LastErrorClass: "rate"}},
		"success after error": {ModelClient{Enabled: true, LastSuccessAt: &t1,
			LastErrorAt: &t0, LastErrorClass: "rate"},
			ModelStatus{Configured: true, Answered: true}},
		"same instant": {ModelClient{Enabled: true, LastSuccessAt: &t0, LastErrorAt: &t0,
			LastErrorClass: "timeout"}, ModelStatus{Configured: true, LastErrorClass: "timeout"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ModelStatusOf(tc.in); got != tc.want {
				t.Fatalf("ModelStatusOf(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// Choosing "Watch only" in the wizard finishes the Grant more step while
// the trust level stays observation; without the choice it stays open.
func TestChecklistGrantStepDoneWhenAutonomyChosen(t *testing.T) {
	open := step(Checklist(ChecklistInput{TrustLevel: LevelObservation}), StepGrantMore)
	if open.Done {
		t.Fatalf("observation without a choice = %+v, want not done", open)
	}
	chosen := step(Checklist(ChecklistInput{TrustLevel: LevelObservation,
		AutonomyChosen: true}), StepGrantMore)
	if !chosen.Done || !strings.Contains(chosen.Detail, "Watch only") {
		t.Fatalf("observation chosen = %+v, want done naming Watch only", chosen)
	}
	unknown := step(Checklist(ChecklistInput{AutonomyChosen: true}), StepGrantMore)
	if unknown.Done {
		t.Fatalf("unknown trust level with a choice = %+v, want not done", unknown)
	}
}
