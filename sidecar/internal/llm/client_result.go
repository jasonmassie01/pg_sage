package llm

import (
	"context"
	"fmt"
	"strings"
)

// Response is one chat completion with its finish signal.
type Response struct {
	Content      string
	Tokens       int
	FinishReason string
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
// Response says it was cut, so prose callers can tell the reader.
func (c *Client) ChatResult(
	ctx context.Context, system, user string, maxTokens int,
) (Response, error) {
	requestCtx, cfg, generation, finish := c.beginRequest(ctx)
	defer finish()
	probe, err := c.checkAvailable(ctx, cfg)
	if err != nil {
		return Response{}, err
	}
	defer c.endProbe(probe)
	maxTokens = normalizedMaxTokens(cfg.Model, maxTokens)
	if err := preflight(cfg, estimateTokens(system, user), maxTokens); err != nil {
		return Response{}, err
	}
	throttleKey := requestThrottleKey(cfg, system, user)
	if err := c.acquireThrottle(throttleKey, cfg.CooldownSeconds); err != nil {
		return Response{}, err
	}
	throttleSuccess := false
	defer func() { c.releaseThrottle(throttleKey, throttleSuccess) }()
	external := externalReservation(system, user, maxTokens)
	reservation, err := c.reserveBudget(cfg, maxTokens, external)
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
		return Response{}, fmt.Errorf("LLM disabled or reconfigured during request")
	}

	c.recordSuccess()
	tokens := usageTokens(chatResp, system, user)
	c.reconcileBudget(reservation, tokens)
	reconciled = true
	r := newResponse(chatResp, tokens)
	if strings.TrimSpace(r.Content) == "" {
		r.Content = ""
		return r, fmt.Errorf("%w (finish_reason=%s)", ErrEmptyResponse, r.FinishReason)
	}
	throttleSuccess = true
	c.repairIfTruncated(&r, system)
	return r, nil
}

// newResponse reads the first choice of resp. Providers report a stop at
// the output token limit as "length" (OpenAI-compatible) or "max_tokens".
func newResponse(resp *ChatResponse, tokens int) Response {
	reason := resp.Choices[0].FinishReason
	return Response{
		Content:      resp.Choices[0].Message.Content,
		Tokens:       tokens,
		FinishReason: reason,
		Truncated:    reason == "length" || reason == "max_tokens",
	}
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
