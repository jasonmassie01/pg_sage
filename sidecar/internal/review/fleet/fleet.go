// Package fleet is Sage Review's fleet tiering (review-04b). A full
// agentic review of every member of a large fleet costs more than the
// databases do, so a fleet pass works in four tiers:
//
//  1. A deterministic rollup of every member (grade, coverage, candidates
//     by generator, the idle rule, size and, with operator rates, cost).
//  2. A low-cost model triage over the rollup, in batches, naming outliers
//     with cited rollup ids; without a usable answer the triage is
//     deterministic (grade drops, candidate impact, cost).
//  3. One fleet-level review whose answers are computed in code (which
//     members are abandoned, which share an anti-pattern from the same
//     generator kind and schema fingerprint, what retiring the idle ones
//     would save); a model only writes its summary.
//  4. Deep reviews for the top outliers only, at most review.fleet_deep_max
//     per calendar month (UTC), their tokens split by need.
//
// Everything here reads; a deep review is fired as a fleet_outlier trigger
// in the member's own review queue, and a review only reads and proposes.
// Model calls carry the purposes review_triage and review_fleet, so the
// usage ledger (sage.llm_usage) attributes each tier's spend.
package fleet

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Usage-ledger purposes of the three model tiers.
const (
	PurposeTriage = "review_triage"
	PurposeFleet  = "review_fleet"
	PurposeDeep   = "review"
)

const (
	// DefaultDeepMax is review.fleet_deep_max's default.
	DefaultDeepMax = 30
	// DeepTokensPerReview is the average token allowance of one deep
	// review; the month's total for the picked outliers is split by need.
	DeepTokensPerReview = 200_000
	// TriageBatchTokens bounds one triage call's prompt (estimated at four
	// bytes a token).
	TriageBatchTokens = 50_000
)

// Where a triage outlier or a summary came from.
const (
	ModeModel         = "model"
	ModeDeterministic = "deterministic"
	ModeMixed         = "mixed"
	SourceTemplate    = "template"
)

// State is a member's place in a pass.
type State string

// Member states, forward only: rollup -> triaged -> deep_scheduled ->
// reviewed.
const (
	StateRollup        State = "rollup"
	StateTriaged       State = "triaged"
	StateDeepScheduled State = "deep_scheduled"
	StateReviewed      State = "reviewed"
)

// CanMove reports whether a member may move from one state to the next.
func CanMove(from, to State) bool {
	switch from {
	case StateRollup:
		return to == StateTriaged
	case StateTriaged:
		return to == StateDeepScheduled
	case StateDeepScheduled:
		return to == StateReviewed
	}
	return false
}

// Pass statuses.
const (
	PassRunning = "running"
	PassDone    = "done"
	PassHalted  = "halted"
	PassFailed  = "failed"
)

// Errors, each distinguishable with errors.Is.
var (
	// ErrState refuses a state move the member is not in a state for.
	ErrState = errors.New("review fleet: member is not in the state the move needs")
	// ErrNoLedger refuses a pass without a ledger, source or scheduler.
	ErrNoLedger = errors.New("review fleet: runner needs a ledger, a source and a scheduler")
)

// Model is a chat model (llm.Client satisfies it).
type Model interface {
	ChatResult(ctx context.Context, system, user string, maxTokens int) (llm.Response, error)
	Model() string
}

// Usage is one tier's model spend. CostUSD is nil when the model has no
// operator price (never a guess).
type Usage struct {
	Calls            int      `json:"calls"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	Model            string   `json:"model,omitempty"`
}

// add charges one call's reply.
func (u *Usage) add(model string, r llm.Response, price llm.PriceFunc) {
	u.Calls++
	u.Model = model
	prompt, completion := int64(r.PromptTokens), int64(r.CompletionTokens)
	if prompt == 0 && completion == 0 {
		prompt = int64(r.Tokens)
	}
	u.PromptTokens += prompt
	u.CompletionTokens += completion
	if cost, ok := llm.CostOf(price, model, prompt, completion, 0); ok {
		total := cost
		if u.CostUSD != nil {
			total += *u.CostUSD
		}
		u.CostUSD = &total
	}
}

// errorClass names a model error: the provider's class (llm.ErrorClass),
// timeout for a deadline, canceled, or error.
func errorClass(err error) string {
	if c := llm.ErrorClass(err); c != "" {
		return c
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}
