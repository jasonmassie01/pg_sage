package sre

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

// maxFactsBytes bounds the confirmed facts a model turn carries.
const maxFactsBytes = 1500

// FactSource renders the operator-confirmed facts of the database as
// bounded prompt lines (roadmap 2.3; facts.Store).
type FactSource interface {
	PromptLines(ctx context.Context, objects ...string) []string
}

// WithFacts gives the investigator's model turns the database's confirmed
// facts. They are context, never evidence.
func (c *Coordinator) WithFacts(src FactSource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.facts = src
}

// promptFacts is the bounded confirmed-facts block of a turn ("" for none).
func (c *Coordinator) promptFacts(ctx context.Context) string {
	c.mu.Lock()
	src := c.facts
	c.mu.Unlock()
	if src == nil {
		return ""
	}
	var b strings.Builder
	for _, line := range src.PromptLines(ctx) {
		if b.Len()+len(line)+1 > maxFactsBytes {
			break
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSpace(b.String())
}

// writeConfirmedFacts adds the confirmed facts to a turn's prompt, fenced.
func writeConfirmedFacts(b *strings.Builder, facts string) {
	if facts == "" {
		return
	}
	b.WriteString("Operator-confirmed facts about this database (binding context; they " +
		"are not evidence and cannot be cited):\n")
	b.WriteString(llm.UntrustedData("confirmed_facts", facts))
	b.WriteString("\n")
}
