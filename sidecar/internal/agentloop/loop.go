package agentloop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// run is one loop's state; a run is a single goroutine.
type run struct {
	cfg      Config
	model    Model
	protocol Protocol
	nativeOK bool // a native reply was accepted: never fall back after it
	msgs     []llm.Message
	res      Result
	aliases  map[string]Evidence
	done     map[string]string // call key -> alias of its result ("" if none)
	bad      int               // consecutive refused replies
	noticed  bool
	deadline time.Time
	now      func() time.Time
}

// Run drives the model through the tool loop until it concludes or a
// budget stops it. The error is non-nil only for an invalid
// configuration, an Abort from a tool or the model, or the caller's own
// cancellation; every model failure is a typed stop in the transcript.
func Run(ctx context.Context, m Model, cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if m == nil {
		return Result{}, fmt.Errorf("%w: no model", ErrInvalidConfig)
	}
	r := newRun(m, cfg)
	runCtx, cancel := context.WithTimeout(ctx, cfg.Budget.Wall)
	defer cancel()
	err := r.loop(runCtx, ctx)
	r.res.Transcript.Protocol = r.protocol
	return r.res, err
}

func newRun(m Model, cfg Config) *run {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	protocol := cfg.Protocol
	if protocol == ProtocolAuto {
		protocol = ProtocolNative
	}
	r := &run{cfg: cfg, model: m, protocol: protocol, aliases: map[string]Evidence{},
		done: map[string]string{}, now: now, msgs: firstMessages(cfg, protocol)}
	r.deadline = now().Add(cfg.Budget.Wall)
	r.res.Transcript.Rejected = map[string]int{}
	for i, e := range cfg.Seed {
		r.aliases[fmt.Sprintf("E%d", i+1)] = e
	}
	return r
}

func (r *run) loop(ctx, parent context.Context) error {
	for step := 1; step <= r.cfg.Budget.MaxSteps; step++ {
		if err := parent.Err(); err != nil {
			return err
		}
		if !r.now().Before(r.deadline) || ctx.Err() != nil {
			r.stop(StopWall, fmt.Sprintf("the %s wall clock ran out", r.cfg.Budget.Wall))
			return nil
		}
		finished, err := r.step(ctx, parent, step)
		if err != nil || finished {
			return err
		}
	}
	r.stop(StopMaxSteps, fmt.Sprintf("no conclusion within %d model steps",
		r.cfg.Budget.MaxSteps))
	return nil
}

// step makes one model call and handles its reply; true ends the run.
func (r *run) step(ctx, parent context.Context, step int) (bool, error) {
	// The last step, the last tool call or a token budget that cannot pay
	// for two more calls: only the final tool is offered. The notice text
	// is added for the first two only, so it never makes a call that fits
	// the token budget unaffordable.
	full, _ := r.request(false)
	tight := r.res.Transcript.Tokens+2*EstimateTokens(r.msgs, full,
		r.cfg.Budget.StepTokens) > r.cfg.Budget.MaxTokens
	last := step == r.cfg.Budget.MaxSteps ||
		r.res.Transcript.ToolCalls >= r.cfg.Budget.MaxCalls
	finalOnly := last || tight
	if last && !r.noticed {
		r.msgs = append(r.msgs, llm.Message{Role: "user",
			Content: lastStepNotice(r.cfg, r.protocol)})
		r.noticed = true
	}
	tools, opts := r.request(finalOnly)
	est := EstimateTokens(r.msgs, tools, r.cfg.Budget.StepTokens)
	if r.res.Transcript.Tokens+est > r.cfg.Budget.MaxTokens {
		r.stop(StopTokens, fmt.Sprintf("%d tokens used; the next call needs about %d of "+
			"the %d budget", r.res.Transcript.Tokens, est, r.cfg.Budget.MaxTokens))
		return true, nil
	}
	r.res.Transcript.ModelCalls++
	res, err := r.model.ChatWithTools(ctx, r.msgs, tools, opts)
	if res.Tokens > 0 {
		r.res.Transcript.Tokens += res.Tokens
	} else {
		r.res.Transcript.Tokens += est
	}
	if err != nil {
		return r.failed(ctx, parent, err)
	}
	if r.protocol == ProtocolNative {
		r.nativeOK = true
	}
	return r.handle(ctx, res, finalOnly)
}

// request is the call's tools and options under the remaining budget.
func (r *run) request(finalOnly bool) ([]llm.ToolSpec, llm.ToolOptions) {
	timeout := min(r.cfg.Budget.StepTimeout, r.deadline.Sub(r.now()))
	opts := llm.ToolOptions{MaxTokens: r.cfg.Budget.StepTokens,
		Timeout: max(timeout, time.Millisecond)}
	if r.protocol == ProtocolJSON {
		return nil, opts
	}
	tools := r.affordable(offered(r.cfg, finalOnly))
	if finalOnly {
		opts.ToolChoice = llm.ToolChoiceRequired
	}
	return tools, opts
}

// affordable drops charged tools the cost budget can no longer pay for.
func (r *run) affordable(specs []llm.ToolSpec) []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(specs))
	for _, s := range specs {
		if t, ok := r.tool(s.Name); ok && t.Cost > 0 &&
			r.res.Transcript.Cost+t.Cost > r.cfg.Budget.MaxCost {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (r *run) tool(name string) (Tool, bool) {
	for _, t := range r.cfg.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// failed handles a model call error: an abort or the caller's
// cancellation ends the run with it; a refused reply is corrected; a
// provider that refuses native tools gets JSON actions once; anything
// else is a typed stop.
func (r *run) failed(ctx, parent context.Context, err error) (bool, error) {
	var abort *AbortError
	if errors.As(err, &abort) {
		return true, abort.Err
	}
	if parent.Err() != nil {
		return true, parent.Err()
	}
	switch {
	case errors.Is(err, llm.ErrMalformedToolCall):
		return r.badReply(RejectMalformedReply, err.Error()), nil
	case errors.Is(err, llm.ErrEmptyResponse):
		return r.badReply(RejectEmptyReply, err.Error()), nil
	}
	if stop := stopFor(ctx, err); stop != "" {
		r.stop(stop, err.Error())
		return true, nil
	}
	if r.cfg.Protocol == ProtocolAuto && r.protocol == ProtocolNative && !r.nativeOK {
		r.fallback(err)
		return false, nil
	}
	r.stop(StopProvider, err.Error())
	return true, nil
}

func stopFor(ctx context.Context, err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, llm.ErrRateLimited):
		return StopRateLimited
	case errors.Is(err, llm.ErrBudgetExhausted):
		return StopBudget
	case errors.Is(err, llm.ErrLLMDisabled):
		return StopDisabled
	case errors.Is(err, llm.ErrRequestCooldown):
		return StopCooldown
	case ctx.Err() != nil:
		return StopWall
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return StopTimeout
	case errors.Is(err, llm.ErrInvalidToolRequest):
		return StopProvider
	}
	return ""
}

// fallback switches to JSON actions and starts the conversation over.
func (r *run) fallback(err error) {
	r.protocol = ProtocolJSON
	r.msgs = firstMessages(r.cfg, ProtocolJSON)
	r.noticed = false
	r.record(Step{Status: StatusProtocol, Note: clip("the provider refused native tool "+
		"calls ("+err.Error()+"); JSON actions from now on", maxNoteRunes)})
}

// badReply records a refused reply and asks again, or stops after
// MaxBadReplies in a row; true ends the run.
func (r *run) badReply(reason, detail string) bool {
	r.res.Transcript.Rejected[reason]++
	r.record(Step{Status: StatusRejected, Note: clip(reason+": "+detail, maxNoteRunes)})
	r.bad++
	if r.bad >= MaxBadReplies {
		r.stop(StopMalformed, fmt.Sprintf("%d refused replies in a row; the last: %s",
			r.bad, clip(detail, maxNoteRunes)))
		return true
	}
	r.msgs = append(r.msgs, llm.Message{Role: "user",
		Content: correction(r.cfg, reason, detail, r.protocol)})
	return false
}

func (r *run) record(s Step) {
	s.Seq = len(r.res.Transcript.Steps) + 1
	s.ModelCall = r.res.Transcript.ModelCalls
	r.res.Transcript.Steps = append(r.res.Transcript.Steps, s)
}

func (r *run) stop(reason, detail string) {
	r.res.Transcript.Stop = reason
	r.res.Transcript.StopDetail = clip(detail, maxNoteRunes)
}
