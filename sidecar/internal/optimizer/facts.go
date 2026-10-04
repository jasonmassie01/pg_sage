package optimizer

import (
	"context"
	"strings"
)

// maxFactsSectionChars bounds the confirmed-facts section of a prompt.
const maxFactsSectionChars = 1600

// FactSource renders the operator-confirmed facts about objects as bounded
// prompt lines (roadmap 2.3; facts.Store). Proposed facts never appear.
type FactSource interface {
	PromptLines(ctx context.Context, objects ...string) []string
}

// WithFacts gives the optimizer's prompts the database's confirmed facts.
// They inform the model; the policy gate enforces them either way.
func WithFacts(src FactSource) func(*Optimizer) {
	return func(o *Optimizer) { o.facts = src }
}

// confirmedFacts are the confirmed facts about tc's table.
func (o *Optimizer) confirmedFacts(ctx context.Context, tc TableContext) []string {
	if o.facts == nil {
		return nil
	}
	return o.facts.PromptLines(ctx, promptIdent(tc.Schema)+"."+promptIdent(tc.Table))
}

// promptIdent quotes an identifier unless it reads back unchanged bare.
func promptIdent(name string) string {
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		}
	}
	return name
}

// writeConfirmedFacts adds the operator-confirmed facts, within
// maxFactsSectionChars.
func writeConfirmedFacts(b *strings.Builder, lines []string) {
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n### Operator-confirmed facts (binding: respect them)\n")
	used := 0
	for _, line := range lines {
		if used+len(line)+1 > maxFactsSectionChars {
			break
		}
		b.WriteString(line + "\n")
		used += len(line) + 1
	}
}
