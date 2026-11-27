package llm

import (
	"context"
	"regexp"
)

// llm-04: a call's purpose (briefing, explain, query_tuning, ...) rides on
// its context, so the usage ledger can say what spent the tokens without
// changing the Chat or ChatWithTools signatures.

const (
	// PurposeUnlabeled is recorded for a call whose context has no purpose.
	PurposeUnlabeled = "unlabeled"
	// PurposeOverflow is recorded once the accumulator holds its maximum
	// number of keys (DefaultUsageMaxKeys).
	PurposeOverflow = "overflow"
)

// purposePattern is a lower-case identifier of at most 63 characters. The
// budget counters' "_total" does not match, so no label can collide with
// fleet-10's reservation rows.
var purposePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

type purposeKey struct{}

// ValidPurpose reports whether purpose is a usable label.
func ValidPurpose(purpose string) bool { return purposePattern.MatchString(purpose) }

// WithPurpose labels the calls made with ctx. An invalid label is ignored:
// the context keeps the purpose it had, or none.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	if !ValidPurpose(purpose) {
		return ctx
	}
	return context.WithValue(ctx, purposeKey{}, purpose)
}

// WithDefaultPurpose labels ctx only when no caller labelled it yet: a
// shared transport (the agent loop) names itself without hiding the
// feature that drives it.
func WithDefaultPurpose(ctx context.Context, purpose string) context.Context {
	if _, ok := ctx.Value(purposeKey{}).(string); ok {
		return ctx
	}
	return WithPurpose(ctx, purpose)
}

// PurposeOf is ctx's purpose, or PurposeUnlabeled.
func PurposeOf(ctx context.Context) string {
	if ctx == nil {
		return PurposeUnlabeled
	}
	if purpose, ok := ctx.Value(purposeKey{}).(string); ok {
		return purpose
	}
	return PurposeUnlabeled
}

// estimateMessages is the prompt estimate of a tool request's messages,
// for the ledger split when the provider reports no usage.
func estimateMessages(msgs []Message) int {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Content)
		for _, tc := range m.ToolCalls {
			parts = append(parts, string(tc.Arguments))
		}
	}
	return estimateTokens(parts...)
}
