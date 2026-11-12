package llm

import (
	"context"
	"errors"
	"time"
)

// observability-05: every Chat, ChatResult and ChatWithTools call reports
// one Outcome to the client's observer, so the sidecar can count requests,
// errors and latency per client before the breaker ever opens.

// Result is the class of one call, from llm-02's error classes.
type Result string

const (
	ResultOK            Result = "ok"
	ResultError         Result = "error"
	ResultTimeout       Result = "timeout"
	ResultRateLimited   Result = "rate_limited"
	ResultBudgetRefused Result = "budget_refused"
	ResultCircuitOpen   Result = "circuit_open"
	ResultEmpty         Result = "empty"
	ResultCancelled     Result = "cancelled"
)

// Results lists every Result in a fixed order.
func Results() []Result {
	return []Result{ResultOK, ResultError, ResultTimeout, ResultRateLimited,
		ResultBudgetRefused, ResultCircuitOpen, ResultEmpty, ResultCancelled}
}

// Outcome is one call as the observer sees it. Tokens is what the call
// was charged; PromptTokens and CompletionTokens are the provider's
// breakdown (0 when it reported none).
type Outcome struct {
	Result           Result
	Duration         time.Duration
	PromptTokens     int
	CompletionTokens int
	Tokens           int
	// llm-04 attribution: the provider's reasoning tokens (part of the
	// completion), the context's purpose (PurposeOf) and the client's
	// model. Estimated: the provider split no tokens, so Tokens is split
	// as EstimatedPromptTokens of prompt and the rest completion.
	ReasoningTokens       int
	Purpose               string
	Model                 string
	Estimated             bool
	EstimatedPromptTokens int
}

// callUsage is what one call charged, for its Outcome.
type callUsage struct {
	prompt, completion, reasoning, tokens int
	// estimatedPrompt is the request's own token estimate, used when the
	// provider reported no split.
	estimatedPrompt int
}

// SetObserver sets the function every counted call reports to; nil
// removes it. It is safe to call while the client is in use. The
// observer runs on the caller's goroutine and must not block.
func (c *Client) SetObserver(observe func(Outcome)) {
	if observe == nil {
		c.observer.Store(nil)
		return
	}
	c.observer.Store(&observe)
}

// observe reports one finished call. A disabled client and the per-work
// item cooldown are not requests and report nothing.
func (c *Client) observe(ctx context.Context, start time.Time, err error, u callUsage) {
	observe := c.observer.Load()
	if observe == nil {
		return
	}
	result, counted := outcomeResult(err)
	if !counted {
		return
	}
	cfg, _ := c.configSnapshot()
	o := Outcome{Result: result, Duration: time.Since(start),
		PromptTokens: u.prompt, CompletionTokens: u.completion, Tokens: u.tokens,
		ReasoningTokens: u.reasoning, Purpose: PurposeOf(ctx), Model: cfg.Model}
	if u.tokens > 0 && u.prompt+u.completion == 0 {
		o.Estimated, o.EstimatedPromptTokens = true, min(u.estimatedPrompt, u.tokens)
	}
	(*observe)(o)
}

// outcomeResult classifies a call's error. counted is false for the
// refusals that are not requests: the client is disabled, or the same
// work item is inside its cooldown.
func outcomeResult(err error) (result Result, counted bool) {
	switch {
	case err == nil:
		return ResultOK, true
	case errors.Is(err, ErrLLMDisabled), errors.Is(err, ErrRequestCooldown):
		return "", false
	case errors.Is(err, ErrEmptyResponse):
		return ResultEmpty, true
	case errors.Is(err, ErrCircuitOpen):
		return ResultCircuitOpen, true
	case errors.Is(err, ErrBudgetExhausted):
		return ResultBudgetRefused, true
	}
	switch errClass(ErrorClass(err)) {
	case classRate:
		return ResultRateLimited, true
	case classTimeout:
		return ResultTimeout, true
	case classCanceled:
		return ResultCancelled, true
	case classNone:
		return contextResult(err), true
	}
	return ResultError, true
}

// contextResult classifies an error that is not a provider failure.
func contextResult(err error) Result {
	switch {
	case errors.Is(err, context.Canceled):
		return ResultCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return ResultTimeout
	}
	return ResultError
}
