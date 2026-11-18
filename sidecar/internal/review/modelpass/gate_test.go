package modelpass

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// Review fix (review-03a): the gate's model choice (fallback_to_general,
// a spent budget, an open breaker) and max_output_tokens.

// stateModel is a scripted model that reports its budget and breaker.
type stateModel struct {
	*scripted
	exhausted, open bool
}

func (m stateModel) IsBudgetExhausted() bool { return m.exhausted }
func (m stateModel) IsCircuitOpen() bool     { return m.open }

func TestReview_FallbackServesWhenTheReviewModelIsUnusable(t *testing.T) {
	in := input(t, "db1")
	primary := stateModel{scripted: always(answer{}), exhausted: true}
	fallback := always(keepRanked(t, in, "t_a"))
	store := &memStore{}
	d := deps(primary, store)
	d.Fallback, d.FallbackName = fallback, "general-model"
	rep, err := Review(context.Background(), settings(3, "majority"), d, in)
	if err != nil || rep.ModelPass != report.PassDone || ids(rep.Ranked) != "t_a" {
		t.Fatalf("pass %s %q ranked %s (%v), want done on the fallback", rep.ModelPass,
			rep.ModelPassReason, ids(rep.Ranked), err)
	}
	if primary.count() != 0 || fallback.count() != 3 {
		t.Fatalf("calls: primary %d, fallback %d; want 0 and 3", primary.count(),
			fallback.count())
	}
	for _, r := range store.all() {
		if r.Model != "general-model" {
			t.Errorf("run %d stored model %q, want the fallback's name", r.Attempt, r.Model)
		}
	}
}

func TestReview_UnusableModelsSkipWithoutACall(t *testing.T) {
	in := input(t, "db1")
	cases := []struct {
		name   string
		mode   string
		model  agentloop.Model
		fb     agentloop.Model
		status report.PassStatus
		reason string
	}{
		{"spent budget, on", "on", stateModel{scripted: always(answer{}), exhausted: true},
			nil, report.PassSkipped, reasonTokenBudget},
		{"open breaker", "auto", stateModel{scripted: always(answer{}), open: true}, nil,
			report.PassSkipped, "circuit breaker is open"},
		{"typed nil client", "auto", (*llm.Client)(nil), nil, report.PassSkipped, "no model"},
		{"unusable fallback too", "on", nil, disabledModel{}, report.PassFailed, "no model"},
	}
	for _, tc := range cases {
		s := settings(3, "majority")
		s.Mode = tc.mode
		d := deps(tc.model, nil)
		d.Fallback = tc.fb
		rep, err := Review(context.Background(), s, d, in)
		if err != nil || rep.ModelPass != tc.status ||
			!strings.Contains(rep.ModelPassReason, tc.reason) {
			t.Errorf("%s: %s %q (%v), want %s %q", tc.name, rep.ModelPass,
				rep.ModelPassReason, err, tc.status, tc.reason)
			continue
		}
		if sm, ok := tc.model.(stateModel); ok && sm.count() != 0 {
			t.Errorf("%s: %d calls, want none", tc.name, sm.count())
		}
		checkFallback(t, rep, in)
	}
}

func TestBudgetFor_CapsEachCallAtMaxOutputTokens(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, DefaultBudget.StepTokens},
		{-5, DefaultBudget.StepTokens}, {1, 1}, {1000, 1000},
		{DefaultBudget.StepTokens + 1, DefaultBudget.StepTokens}} {
		b := BudgetFor(tc.in)
		if b.StepTokens != tc.want || b.MaxTokens != DefaultBudget.MaxTokens ||
			b.MaxSteps != DefaultBudget.MaxSteps {
			t.Errorf("BudgetFor(%d) = %+v, want step tokens %d over the default", tc.in, b,
				tc.want)
		}
	}
}
