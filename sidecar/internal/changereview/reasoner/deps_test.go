package reasoner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Construction and the stage's contract with the service.

func TestNew_RefusesIncompleteDeps(t *testing.T) {
	model := newFakeLLM(t).client()
	var nilClient *llm.Client
	bad := Bounds{MaxSteps: 0, MaxCalls: 1, MaxCost: 1, Wall: time.Second, MaxTokens: 1,
		StepTokens: 1, StepTimeout: time.Second}
	cases := map[string]Deps{
		"no tools":         {Model: model, Budget: &memBudget{}},
		"model, no budget": {Model: model, Tools: newFakeTools()},
		"bad bounds":       {Tools: newFakeTools(), Bounds: bad},
		"bad protocol": {Tools: newFakeTools(), Model: model, Budget: &memBudget{},
			Protocol: agentloop.Protocol("smoke")},
	}
	for name, d := range cases {
		if _, err := New(d); !errors.Is(err, changereview.ErrInvalid) {
			t.Errorf("%s: New = %v, want ErrInvalid", name, err)
		}
	}
	r, err := New(Deps{Tools: newFakeTools(), Model: nilClient})
	if err != nil {
		t.Fatalf("a nil *llm.Client is no model: %v", err)
	}
	res := reason(t, r, testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	if res.ModelStatus != changereview.ModelNoModel {
		t.Fatalf("nil client: status %q", res.ModelStatus)
	}
}

func TestDefaultBoundsAreTheSpecBudget(t *testing.T) {
	b := DefaultBounds()
	if b.MaxSteps != 8 || b.MaxCalls != 16 || b.MaxCost != 10 || b.Wall != 75*time.Second ||
		b.StepTokens != 3000 || b.StepTimeout != 30*time.Second {
		t.Fatalf("default bounds = %+v", b)
	}
	if MaxStatements != 10 || MaxFindings != 8 || MaxClaims != 4 || MaxModelNotDetermined != 6 ||
		MaxSuggestionBytes != 2048 || LineTolerance != 3 || MaxExcerptBytes != 4096 {
		t.Fatal("submit_review limits differ from the spec")
	}
}

func TestReason_EmptyInputWithoutModel(t *testing.T) {
	res := reason(t, newTestReasoner(t, Deps{}), changereview.Input{}, changereview.Analysis{})
	if res.ModelStatus != changereview.ModelNoModel || len(res.Findings) != 0 ||
		len(res.Statements) != 0 || len(res.Evidence) != 0 {
		t.Fatalf("empty review = %+v", res)
	}
}

func TestReason_ToolsAreOfferedWithTheSpecCosts(t *testing.T) {
	r := newTestReasoner(t, Deps{Model: newFakeLLM(t).client()})
	s := r.newSession(testInput(controllerDiff, ""), safeAnalysis())
	costs := map[string]int{}
	for _, tool := range s.tools() {
		costs[tool.Name] = tool.Cost
	}
	want := map[string]int{ToolInspect: 1, ToolQueriesTouching: 0, ToolTableContext: 0,
		ToolWhatIf: 1, ToolDiffExcerpt: 0}
	if len(costs) != len(want) {
		t.Fatalf("tools %v, want %v", costs, want)
	}
	for name, cost := range want {
		if c, ok := costs[name]; !ok || c != cost {
			t.Errorf("tool %s cost %d (%v), want %d", name, c, ok, cost)
		}
	}
}

// Concurrent reviews on one Reasoner share only its dependencies.
func TestReason_ConcurrentReviewsOnOneReasoner(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit(), emptySubmit(), emptySubmit(), emptySubmit())
	r := newTestReasoner(t, Deps{Model: fake.client()})
	done := make(chan changereview.Reasoning, 4)
	for i := 0; i < 4; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := r.Reason(ctx, testInput(controllerDiff, ""), safeAnalysis())
			if err != nil {
				t.Errorf("Reason: %v", err)
			}
			done <- res
		}()
	}
	for i := 0; i < 4; i++ {
		if res := <-done; res.ModelStatus != changereview.ModelOK {
			t.Fatalf("status %q", res.ModelStatus)
		}
	}
	if fake.calls() != 4 {
		t.Fatalf("%d requests, want one per review", fake.calls())
	}
}
