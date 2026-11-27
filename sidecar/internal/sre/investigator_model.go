package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
)

// investigatorModel is the loop's model: each call heartbeats the lease
// while it runs, is reserved durably before provider I/O (the plan's
// per-investigation cap, the daily allocations), is bounded by that
// reservation in the client, and settles with the usage the provider
// reported. A lost lease or an unavailable store aborts the run; a
// refused reservation is the loop's budget stop.
type investigatorModel struct{ s *investigatorSession }

// maxInvestigatorReasoning is a thinking model's reasoning allowance per
// call, reserved on top of the completion cap.
const maxInvestigatorReasoning = 2048

func (m investigatorModel) ChatWithTools(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	s := m.s
	lease, err := s.c.store.Heartbeat(ctx, s.lease)
	if err != nil {
		return llm.ToolResult{}, agentloop.Abort(err)
	}
	s.lease = lease
	callCtx, stop := s.c.keepAlive(ctx, s.lease)
	res, err := s.call(callCtx, msgs, tools, opts)
	lease, hbErr := stop()
	s.lease = lease
	switch {
	case hbErr != nil:
		return res, agentloop.Abort(hbErr)
	case ctx.Err() != nil:
		return res, ctx.Err()
	case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrMetadataUnavailable):
		return res, agentloop.Abort(err)
	case errors.Is(err, ErrUsageExceeded) && hasReply(res):
		s.c.logFn("WARN", "sre: investigation %s: the provider used more tokens than "+
			"reserved: %v", s.inv.ID, err)
		return res, nil
	case errors.Is(err, ErrBudgetExhausted):
		return res, fmt.Errorf("%w: %v", llm.ErrBudgetExhausted, err)
	}
	return res, err
}

// minFinalTokens is the smallest answer a final-only retry asks for.
const minFinalTokens = 256

// reasoning is the reasoning allowance each call reserves: the loop's
// ReasoningPerCall and the reservation share it.
func (s *investigatorSession) reasoning() int {
	if s.c.model.ThinkingModel() {
		return maxInvestigatorReasoning
	}
	return 0
}

// call is one reserved model call. A call the investigation's own cap
// refuses while it offered more than the final tool (a resumed run finds
// earlier runs' tokens held, which its loop does not count) is asked once
// more as final-only, its answer capped at what remains above the
// estimate (at least minFinalTokens). A daily-allocation refusal, JSON
// actions and a call that was already final-only stop on the budget.
func (s *investigatorSession) call(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	res, err := s.reservedCall(ctx, msgs, tools, opts)
	var refused *capRefusal
	if !errors.As(err, &refused) || opts.ToolChoice == llm.ToolChoiceRequired {
		return res, err
	}
	final := finalTool(tools)
	if final == nil {
		return res, err
	}
	left := int(refused.Cap-refused.Held) - agentloop.EstimateTokens(msgs, final, 0) -
		s.reasoning()
	opts.MaxTokens = min(opts.MaxTokens, max(minFinalTokens, left))
	opts.ToolChoice = llm.ToolChoiceRequired
	s.c.logFn("INFO", "sre: investigation %s: %v; asking once for the conclusion "+
		"within %d answer tokens", s.inv.ID, err, opts.MaxTokens)
	return s.reservedCall(ctx, msgs, final, opts)
}

// finalTool is the conclusion tool among those offered, if any.
func finalTool(tools []llm.ToolSpec) []llm.ToolSpec {
	for _, t := range tools {
		if t.Name == ToolSubmit {
			return []llm.ToolSpec{t}
		}
	}
	return nil
}

// reservedCall reserves the call exactly as the loop estimated it, calls
// the model bounded by the reservation and settles it.
//
//llm:transport internal/agentloop.systemPrompt
func (s *investigatorSession) reservedCall(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	s.calls++
	input := int64(max(1, agentloop.EstimateTokens(msgs, tools, 0)))
	res, err := s.c.invStore.ReserveInvestigator(ctx, s.lease, TokenRequest{
		Input: input, Output: int64(max(1, opts.MaxTokens)), Reasoning: int64(s.reasoning()),
		RequestKey: fmt.Sprintf("inv-f%d-%d", s.lease.Fence, s.calls)}, s.plan.MaxTokens)
	if err != nil {
		return llm.ToolResult{}, err
	}
	if res, err = s.c.store.MarkDispatched(ctx, s.lease.Scope, res); err != nil {
		return llm.ToolResult{}, err
	}
	budget := NewCallBudget(res)
	opts.Budget, opts.MaxTokens = budget, int(res.Output)
	opts.ReasoningTokens = int(res.Reasoning)
	pctx := llm.WithPurpose(ctx, "sre_investigation") // llm-04: usage ledger label
	result, callErr := s.c.model.ChatWithTools(pctx, msgs, tools, opts)
	_, settleErr := s.c.store.SettleModel(context.WithoutCancel(ctx), s.lease.Scope, res,
		usageOf(budget, result, callErr, res))
	return result, errors.Join(callErr, settleErr)
}
