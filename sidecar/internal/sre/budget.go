package sre

import (
	"context"
	"errors"
	"sync"

	"github.com/pg-sage/sidecar/internal/llm"
)

// CallBudget is the per-investigation budget on the existing LLM budget
// interface (llm.Budgeter): one model call may hold at most its durable
// reservation. It also records whether the client charged it, which
// happens only right before provider I/O.
type CallBudget struct {
	mu         sync.Mutex
	limit      int64
	net        int64
	dispatched bool
}

var _ llm.Budgeter = (*CallBudget)(nil)

// NewCallBudget bounds one call by a reservation's input, answer and
// reasoning tokens.
func NewCallBudget(res Reservation) *CallBudget {
	return &CallBudget{limit: res.Input + res.Output + res.Reasoning}
}

// CanSpend reports whether tokens fit in what remains.
func (b *CallBudget) CanSpend(tokens int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.net+int64(tokens) <= b.limit
}

// Spend applies a usage delta; a positive charge marks the call as
// dispatched.
func (b *CallBudget) Spend(tokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if tokens > 0 {
		b.dispatched = true
	}
	b.net += int64(tokens)
}

// Dispatched reports whether the client reached provider dispatch.
func (b *CallBudget) Dispatched() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dispatched
}

// ModelTurn is one model call inside an investigation.
type ModelTurn struct {
	Messages   []llm.Message
	Tools      []llm.ToolSpec
	Options    llm.ToolOptions
	Input      int64 // tokens reserved for the prompt
	Output     int64 // tokens reserved for the completion (caps MaxTokens)
	Reasoning  int64 // reasoning allowance of a thinking model (0 otherwise)
	RequestKey string
}

// TurnOutcome is the model result and the settled reservation.
type TurnOutcome struct {
	Result      llm.ToolResult
	Reservation Reservation
}

// RunModelTurn reserves the turn durably, marks it in flight, calls the
// model bounded by the reservation, and settles: known usage when the
// provider reported it (or proved nothing was sent), otherwise the full
// hold stays (unknown). The caller's errors and settlement errors are
// both returned.
func RunModelTurn(ctx context.Context, st ModelStore, lease Lease, client *llm.Client,
	turn ModelTurn) (TurnOutcome, error) {
	if client == nil {
		return TurnOutcome{}, llm.ErrLLMDisabled
	}
	res, err := st.ReserveModel(ctx, lease, TokenRequest{Input: turn.Input,
		Output: turn.Output, Reasoning: turn.Reasoning, RequestKey: turn.RequestKey})
	if err != nil {
		return TurnOutcome{}, err
	}
	if res, err = st.MarkDispatched(ctx, lease.Scope, res); err != nil {
		return TurnOutcome{Reservation: res}, err
	}
	budget := NewCallBudget(res)
	opts := turn.Options
	opts.Budget = budget
	if opts.MaxTokens <= 0 || int64(opts.MaxTokens) > res.Output {
		opts.MaxTokens = int(res.Output)
	}
	opts.ReasoningTokens = int(res.Reasoning)
	result, callErr := client.ChatWithTools(ctx, turn.Messages, turn.Tools, opts)
	settled, settleErr := st.SettleModel(context.WithoutCancel(ctx), lease.Scope, res,
		usageOf(budget, result, callErr, res))
	return TurnOutcome{Result: result, Reservation: settled}, errors.Join(callErr, settleErr)
}

// usageOf decides what a finished call cost: nothing when it never
// reached the provider or was rate limited, the reported tokens when
// known, and unknown (full hold) otherwise.
func usageOf(b *CallBudget, r llm.ToolResult, err error, res Reservation) Usage {
	switch {
	case !b.Dispatched(), errors.Is(err, llm.ErrRateLimited):
		return Usage{Known: true}
	case r.Tokens > 0:
		return splitUsage(r, res)
	default:
		return Usage{}
	}
}

// splitUsage records the reported total as input, answer and reasoning:
// the provider's breakdown when it gave one (prompt tokens, reasoning
// tokens, or a total above prompt + completion, as Gemini reports
// thoughts), otherwise the reservation's shape. Reasoning over the
// allowance is recorded as reported.
func splitUsage(r llm.ToolResult, res Reservation) Usage {
	total := int64(r.Tokens)
	in := min(total, res.Input)
	if r.PromptTokens > 0 {
		in = min(total, int64(r.PromptTokens))
	}
	rest := total - in
	var reasoning int64
	switch {
	case r.ReasoningTokens > 0:
		reasoning = min(rest, int64(r.ReasoningTokens))
	case r.PromptTokens > 0 && r.CompletionTokens > 0:
		reasoning = max(0, rest-int64(r.CompletionTokens))
	default:
		reasoning = max(0, rest-res.Output)
	}
	return Usage{Input: in, Output: rest - reasoning, Reasoning: reasoning, Known: true}
}
