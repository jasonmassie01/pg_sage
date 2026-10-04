package sre

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Roadmap 2.3: the investigator's model turn carries the operator-confirmed
// facts of its database, fenced and bounded, and never as evidence.

type sreFacts struct {
	lines []string
	calls int
}

func (f *sreFacts) PromptLines(context.Context, ...string) []string {
	f.calls++
	return f.lines
}

func TestReviewPromptCarriesConfirmedFacts(t *testing.T) {
	scope := newReviewScope(causal.Diagnosis{Family: "wal_retention"}, nil, false)
	scope.facts = "- fact #9: replication slot cdc_orders belongs to debezium."
	got := scope.userPrompt(Investigation{TriggerKind: "signal"})
	if !strings.Contains(got, "Operator-confirmed facts") ||
		!strings.Contains(got, "fact #9: replication slot cdc_orders belongs to debezium") ||
		!strings.Contains(got, "<data") {
		t.Fatalf("prompt lacks fenced facts:\n%s", got)
	}
	scope.facts = ""
	if got := scope.userPrompt(Investigation{}); strings.Contains(got,
		"Operator-confirmed facts") {
		t.Fatalf("empty facts section:\n%s", got)
	}
}

func TestCoordinatorPromptFactsAreBounded(t *testing.T) {
	c := &Coordinator{}
	if got := c.promptFacts(context.Background()); got != "" {
		t.Fatalf("without a source: %q", got)
	}
	src := &sreFacts{lines: []string{"- fact #1: a", "- fact #2: " + strings.Repeat("b", 5000)}}
	c.WithFacts(src)
	got := c.promptFacts(context.Background())
	if src.calls != 1 || !strings.Contains(got, "fact #1: a") || len(got) > maxFactsBytes {
		t.Fatalf("facts %d bytes: %.80q", len(got), got)
	}
}
