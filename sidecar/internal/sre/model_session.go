package sre

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// callResult is a model call's reply, or why there is none.
type callResult struct {
	result    llm.ToolResult
	rejection *ModelRejection
}

// call runs one durable, budgeted model turn while heartbeating the
// lease. Model failures become rejections; a lost lease, an unavailable
// store or the run's own cancellation are returned as errors.
func (s *modelSession) call(ctx context.Context, turn ModelTurn) (callResult, error) {
	lease, err := s.c.store.Heartbeat(ctx, s.lease)
	if err != nil {
		return callResult{}, err
	}
	s.lease = lease
	callCtx, stop := s.c.keepAlive(ctx, s.lease)
	out, err := RunModelTurn(callCtx, s.c.store, s.lease, s.c.model, turn)
	lease, hbErr := stop()
	s.lease = lease
	switch {
	case hbErr != nil:
		return callResult{}, hbErr
	case ctx.Err() != nil:
		return callResult{}, ctx.Err()
	case err == nil:
		return callResult{result: out.Result}, nil
	case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrMetadataUnavailable):
		return callResult{}, err
	case errors.Is(err, ErrUsageExceeded) && hasReply(out.Result):
		s.c.logFn("WARN", "sre: investigation %s: the provider used more tokens than "+
			"reserved: %v", s.lease.InvestigationID, err)
		return callResult{result: out.Result}, nil
	}
	return callResult{rejection: s.classify(err)}, nil
}

func hasReply(r llm.ToolResult) bool { return r.Content != "" || len(r.ToolCalls) > 0 }

// classify names a failed call. A provider error other than the known
// ones switches the repair turn to JSON-schema prompting without tools.
func (s *modelSession) classify(err error) *ModelRejection {
	reason := RejectProvider
	switch {
	case errors.Is(err, llm.ErrLLMDisabled):
		reason = RejectDisabled
	case errors.Is(err, llm.ErrRateLimited):
		reason = RejectRateLimited
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		reason = RejectTimeout
	case errors.Is(err, ErrBudgetExhausted), errors.Is(err, llm.ErrBudgetExhausted):
		reason = RejectBudget
	case errors.Is(err, llm.ErrRequestCooldown):
		reason = RejectCooldown
	case errors.Is(err, llm.ErrEmptyResponse):
		reason = RejectEmpty
	case errors.Is(err, llm.ErrMalformedToolCall):
		reason = RejectMalformed
	case errors.Is(err, llm.ErrInvalidToolRequest), errors.Is(err, ErrInvalidRequest):
		reason = RejectInvalidRequest
	default:
		s.tools = false
	}
	return reject(reason, "%v", err)
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// keepAlive heartbeats the lease while a model call runs, so a slow
// provider cannot let the lease expire under it. A failed heartbeat
// cancels the call. stop ends it and returns the latest lease and the
// heartbeat failure, if any.
func (c *Coordinator) keepAlive(ctx context.Context,
	lease Lease) (context.Context, func() (Lease, error)) {
	callCtx, cancel := context.WithCancel(ctx)
	interval := max(time.Until(lease.Until)/3, 50*time.Millisecond)
	var mu sync.Mutex
	cur, failure := lease, error(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-callCtx.Done():
				return
			case <-ticker.C:
			}
			mu.Lock()
			next, err := c.store.Heartbeat(callCtx, cur)
			if err == nil {
				cur = next
			} else if callCtx.Err() == nil {
				failure = err
			}
			mu.Unlock()
			if err != nil {
				cancel()
				return
			}
		}
	}()
	return callCtx, func() (Lease, error) {
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return cur, failure
	}
}

// rejected records a fallback. A disabled client is not an
// investigation event: it is said once per process.
func (s *modelSession) rejected(ctx context.Context, rej *ModelRejection,
	stage string) error {
	if rej == nil {
		return nil
	}
	if rej.Reason == RejectDisabled {
		NoteModelUnavailable(s.c.notices, s.c.logFn, rej.Detail)
		return nil
	}
	s.c.logFn("INFO", "sre: investigation %s: model %s not used (%s): %s",
		s.lease.InvestigationID, stage, rej.Reason, rej.Detail)
	return s.c.store.RecordEvent(ctx, s.lease, EventModelRejected, map[string]any{
		"reason": rej.Reason, "detail": rej.Detail, "stage": stage,
		"turns": s.inv.ModelTurns + s.turns})
}
