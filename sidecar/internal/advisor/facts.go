package advisor

import (
	"context"
	"strings"
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
	block := strings.Join(a.facts.PromptLines(ctx), "\n")
	if len(block) > maxFactsChars {
		block = block[:strings.LastIndex(block[:maxFactsChars], "\n")+1]
	}
	return context.WithValue(ctx, promptFactsKey{}, strings.TrimSpace(block))
}

// promptFactsFrom is the facts block a review carries ("" for none).
func promptFactsFrom(ctx context.Context) string {
	block, _ := ctx.Value(promptFactsKey{}).(string)
	return block
}
