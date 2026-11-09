package advisor

import (
	"context"
	"strings"
	"unicode/utf8"
)

// maxFactsChars bounds the confirmed facts an advisor prompt carries.
const maxFactsChars = 1500

// FactSource renders the operator-confirmed facts of the database as
// bounded prompt lines (roadmap 2.3; facts.Store).
type FactSource interface {
	PromptLines(ctx context.Context, objects ...string) []string
}

// WithFacts gives every advisor prompt the database's confirmed facts.
func (a *Advisor) WithFacts(src FactSource) { a.facts = src }

type promptFactsKey struct{}

// factsContext carries the confirmed facts of this review to every
// sub-advisor's prompt (chatAdvisor). Without a source ctx is unchanged.
func (a *Advisor) factsContext(ctx context.Context) context.Context {
	if a.facts == nil {
		return ctx
	}
	block := capFacts(strings.Join(a.facts.PromptLines(ctx), "\n"))
	return context.WithValue(ctx, promptFactsKey{}, strings.TrimSpace(block))
}

// capFacts bounds the block at maxFactsChars bytes: at the last whole line
// that fits, or, when the first line alone is longer, at a rune boundary,
// so the prompt never carries a split UTF-8 sequence.
func capFacts(block string) string {
	if len(block) <= maxFactsChars {
		return block
	}
	if i := strings.LastIndex(block[:maxFactsChars], "\n"); i >= 0 {
		return block[:i+1]
	}
	cut := maxFactsChars
	for cut > 0 && !utf8.RuneStart(block[cut]) {
		cut--
	}
	return block[:cut]
}

// promptFactsFrom is the facts block a review carries ("" for none).
func promptFactsFrom(ctx context.Context) string {
	block, _ := ctx.Value(promptFactsKey{}).(string)
	return block
}
