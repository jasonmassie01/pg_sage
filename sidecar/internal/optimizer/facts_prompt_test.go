package optimizer

import (
	"context"
	"strings"
	"testing"
)

// Roadmap 2.3: the optimizer's prompt carries the operator-confirmed facts
// about the table it asks about, bounded, and nothing else.

type stubFacts struct {
	lines   []string
	objects []string
}

func (s *stubFacts) PromptLines(_ context.Context, objects ...string) []string {
	s.objects = append(s.objects, objects...)
	return s.lines
}

func TestFormatPromptCarriesConfirmedFacts(t *testing.T) {
	tc := TableContext{Schema: "public", Table: "thesis",
		ConfirmedFacts: []string{"- fact #12: table public.thesis is owned by the " +
			"application's migrations. Propose an application migration instead."}}
	got := FormatPrompt(tc)
	if !strings.Contains(got, "### Operator-confirmed facts") ||
		!strings.Contains(got, "fact #12: table public.thesis") {
		t.Fatalf("prompt lacks the facts section:\n%s", got)
	}
	tc.ConfirmedFacts = nil
	if got := FormatPrompt(tc); strings.Contains(got, "Operator-confirmed facts") {
		t.Fatalf("an empty facts section was written:\n%s", got)
	}
}

func TestFormatPromptBoundsTheFactsSection(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "- fact #"+strings.Repeat("9", 5)+": "+strings.Repeat("x", 120))
	}
	got := FormatPrompt(TableContext{Schema: "s", Table: "t", ConfirmedFacts: lines})
	start := strings.Index(got, "### Operator-confirmed facts")
	if start < 0 {
		t.Fatal("no facts section")
	}
	section := got[start:]
	if end := strings.Index(section[4:], "###"); end >= 0 {
		section = section[:end+4]
	}
	if len(section) > maxFactsSectionChars+200 {
		t.Fatalf("facts section is %d bytes", len(section))
	}
}

func TestOptimizerAsksFactsAboutTheTable(t *testing.T) {
	src := &stubFacts{lines: []string{"- fact #1: x"}}
	o := &Optimizer{}
	WithFacts(src)(o)
	got := o.confirmedFacts(context.Background(), TableContext{Schema: "app", Table: "orders"})
	if len(got) != 1 || len(src.objects) != 1 || src.objects[0] != "app.orders" {
		t.Fatalf("lines %v, asked about %v", got, src.objects)
	}
	if got := (&Optimizer{}).confirmedFacts(context.Background(),
		TableContext{Schema: "a", Table: "b"}); got != nil {
		t.Fatalf("without a source: %v", got)
	}
}
