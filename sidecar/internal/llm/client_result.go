package llm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// errReconfiguredDuringRequest discards a reply that arrived after a
// Reconfigure or disable. It wraps ErrLLMDisabled, as the tool path does,
// so the observer does not count it as a call (observability-05).
var errReconfiguredDuringRequest = fmt.Errorf(
	"LLM disabled or reconfigured during request: %w", ErrLLMDisabled)

// Response is one chat completion with its finish signal.
type Response struct {
	Content      string
	Tokens       int
	FinishReason string
	// The provider's usage breakdown, as reported (0 when not reported).
	PromptTokens     int
	CompletionTokens int
	// Truncated reports a provider stop at the output token limit
	// (finish_reason "length" or "max_tokens"): the content is cut.
	Truncated bool
	// Repaired reports that a truncated JSON answer was cut back to its
	// last complete value and closed (RepairTruncatedJSON).
	Repaired bool
}

// ChatResult sends a chat completion request. A blank completion returns
// ErrEmptyResponse with the tokens still charged; it is not counted as a
// provider failure by the circuit breaker. A truncated JSON answer (the
// system prompt asks for JSON, or the answer is a JSON document) is
// repaired; truncated prose is returned byte-identical. Either way the
// Response says it was cut, so prose callers can tell the reader. Each
// call reports one Outcome to the client's observer.
func (c *Client) ChatResult(
	ctx context.Context, system, user string, maxTokens int,
) (Response, error) {
	start := time.Now()
	r, err := c.chatResult(ctx, system, user, maxTokens)
	c.observe(ctx, start, err, callUsage{prompt: r.PromptTokens,
		completion: r.CompletionTokens, tokens: r.Tokens,
		estimatedPrompt: estimateTokens(system, user)})
	return r, err
}

func (c *Client) chatResult(
	ctx context.Context, system, user string, maxTokens int,
) (Response, error) {
	requestCtx, cfg, generation, finish := c.beginRequest(ctx)
	defer finish()
	probe, err := c.checkAvailable(ctx, cfg)
	if err != nil {
		return Response{}, err
	}
	defer c.endProbe(probe)
	if maxTokens, err = preflightMaxTokens(cfg, system, user, maxTokens); err != nil {
		return Response{}, err
	}
	throttleKey := requestThrottleKey(cfg, system, user)
	if err := c.acquireThrottle(throttleKey, cfg.CooldownSeconds); err != nil {
		return Response{}, err
	}
	throttleSuccess := false
	defer func() { c.releaseThrottle(throttleKey, throttleSuccess) }()
	reservation, err := c.reserveBudget(cfg, maxTokens,
		externalReservation(system, user, maxTokens))
	if err != nil {
		return Response{}, err
	}
	reconciled := false
	defer func() {
		if !reconciled {
			c.releaseBudget(reservation)
		}
	}()
	chatResp, err := c.sendChat(ctx, requestCtx, cfg, system, user, maxTokens)
	if err != nil {
		c.noteFailure(generation, err, true)
		return Response{}, err
	}
	if !c.generationCurrent(generation) {
		return Response{}, errReconfiguredDuringRequest
	}
	c.recordSuccess()
	tokens := usageTokens(chatResp, system, user)
	c.reconcileBudget(reservation, tokens)
	reconciled = true
	r, err := newResponse(chatResp, tokens)
	if err != nil {
		return r, err
	}
	throttleSuccess = true
	c.repairIfTruncated(&r, system)
	return r, nil
}

// newResponse reads the first choice of resp. Providers report a stop at
// the output token limit as "length" (OpenAI-compatible) or "max_tokens".
// A blank completion returns ErrEmptyResponse with the tokens kept.
func newResponse(resp *ChatResponse, tokens int) (Response, error) {
	reason := resp.Choices[0].FinishReason
	r := Response{
		Content:          resp.Choices[0].Message.Content,
		Tokens:           tokens,
		FinishReason:     reason,
		Truncated:        reason == "length" || reason == "max_tokens",
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
	if strings.TrimSpace(r.Content) == "" {
		r.Content = ""
		return r, fmt.Errorf("%w (finish_reason=%s)", ErrEmptyResponse, r.FinishReason)
	}
	return r, nil
}

// repairIfTruncated logs a truncated answer and repairs it when it is
// JSON. Prose is never rewritten: the repair would turn bracketed prose
// into a fragment.
func (c *Client) repairIfTruncated(r *Response, system string) {
	if !r.Truncated {
		return
	}
	isJSON := promptRequestsJSON(system) || answerIsJSONDocument(r.Content)
	c.logFn("llm", "response truncated (finish_reason=%s, tokens=%d, json=%t)",
		r.FinishReason, r.Tokens, isJSON)
	if !isJSON {
		return
	}
	repaired := RepairTruncatedJSON(r.Content)
	r.Repaired = repaired != r.Content
	r.Content = repaired
}

// answerIsJSONDocument reports an answer that starts as a JSON container,
// so a JSON reply to a prompt that does not say "JSON" is still salvaged.
func answerIsJSONDocument(content string) bool {
	s := strings.TrimSpace(content)
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}
