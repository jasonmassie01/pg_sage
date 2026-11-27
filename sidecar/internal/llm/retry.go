package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// maxAttempts bounds the provider requests one chat call sends for rate
// limits and server errors. The budget is reserved once per call, so the
// retries are never charged again.
const maxAttempts = 4

// defaultRetryDelays is the wait before attempts 2, 3 and 4 when the
// provider sends no Retry-After.
func defaultRetryDelays() []time.Duration {
	return []time.Duration{1 * time.Second, 4 * time.Second, 16 * time.Second}
}

// attemptTimeout is one attempt's deadline: llm.timeout_seconds, and for
// reasoning models at least the optimizer default (120 s), since they
// routinely think past a 30 s chat timeout.
func attemptTimeout(cfg config.LLMConfig) time.Duration {
	seconds := cfg.TimeoutSeconds
	if seconds <= 0 {
		seconds = config.DefaultLLMTimeoutSeconds
	}
	if isThinkingModel(cfg.Model) {
		seconds = max(seconds, config.DefaultOptLLMTimeoutSeconds)
	}
	return time.Duration(seconds) * time.Second
}

// providerReply is one provider answer, read in full (capped at 1 MB).
type providerReply struct {
	status int
	header http.Header
	body   []byte
}

// post sends body to the chat endpoint. Rate limits and server errors are
// re-sent up to attempts times in all, each attempt with its own deadline
// and the waits bounded by requestCtx; the last reply is returned. A
// transport error or timeout is returned as a classified failure at once.
func (c *Client) post(
	ctx, requestCtx context.Context, cfg config.LLMConfig, body []byte,
	attempts int, label string,
) (providerReply, error) {
	for attempt := 1; ; attempt++ {
		reply, err := c.attempt(ctx, requestCtx, cfg, body, label)
		if err != nil || attempt >= attempts || !retryableStatus(reply.status) {
			return reply, err
		}
		wait := c.retryWait(attempt, reply.header)
		if !fitsDeadline(requestCtx, wait) {
			return reply, nil
		}
		select {
		case <-requestCtx.Done():
			return providerReply{}, transportFailure(ctx, requestCtx, label,
				requestCtx.Err())
		case <-time.After(wait):
		}
	}
}

// retryWait is the provider's Retry-After when it sent one, else the
// client's ladder step for the attempt that just failed.
func (c *Client) retryWait(attempt int, header http.Header) time.Duration {
	if wait, ok := parseRetryAfter(header.Get("Retry-After"), time.Now()); ok {
		return wait
	}
	if len(c.retryDelays) == 0 {
		return 0
	}
	return c.retryDelays[min(attempt, len(c.retryDelays))-1]
}

// fitsDeadline reports whether a wait ends before ctx's deadline; waiting
// into the deadline would only turn the provider's answer into a timeout.
func fitsDeadline(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > wait
}

// attempt sends one request under its own attemptTimeout deadline.
func (c *Client) attempt(
	ctx, requestCtx context.Context, cfg config.LLMConfig, body []byte,
	label string,
) (providerReply, error) {
	attemptCtx, cancel := context.WithTimeout(requestCtx, attemptTimeout(cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost,
		chatEndpoint(cfg.Endpoint), bytes.NewReader(body))
	if err != nil {
		return providerReply{}, fmt.Errorf("%s: %w", label, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	if cfg.Auth == "databricks" {
		if err := c.authorizeDatabricks(req); err != nil {
			return providerReply{}, err
		}
	}
	resp, err := c.doRequest(req, cfg)
	if err != nil {
		return providerReply{}, transportFailure(ctx, requestCtx, label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return providerReply{}, transportFailure(ctx, requestCtx,
			"read "+label+" response", err)
	}
	return providerReply{status: resp.StatusCode, header: resp.Header, body: raw}, nil
}

// transportFailure classifies an error without a provider reply. A
// caller cancellation or a Reconfigure is not the provider's failure.
func transportFailure(ctx, requestCtx context.Context, label string, err error) error {
	class := classCanceled
	if shouldRecordProviderFailure(ctx, requestCtx) {
		class = classify(0, nil, err)
	}
	return &providerFailure{class: class, err: providerRequestError(label, err)}
}
