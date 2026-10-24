// Package reasoner is Change Review's model stage (changereview.Reasoner):
// one bounded agentloop run per review over five closed, read-only tools
// backed by the impact engine (inspect_statement, queries_touching,
// table_context, whatif_index, diff_excerpt) and the final submit_review.
//
// The model translates ORM migrations into SQL, extracts the statements
// new data-access code will run, judges the change's intent and writes
// findings in the codebase's idiom. Deterministic post-processing decides
// what of that survives: a statement must parse, plan and point within
// ±3 lines of an added line of the diff; a finding must cite evidence the
// run holds and every number in it must appear there; severities are
// clamped to warning. The model can raise a verdict to review, never
// lower one. The diff, SQL, file names and PR text are untrusted: they
// reach the model only fenced and redacted, and only data-access hunks
// are offered. Without a model the review stays deterministic and an
// ORM-only migration gets CR-20 with its tool's SQL export command.
package reasoner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Tool names; Purpose is the LLM purpose the runtime resolves the model by.
const (
	ToolInspect         = "inspect_statement"
	ToolQueriesTouching = "queries_touching"
	ToolTableContext    = "table_context"
	ToolWhatIf          = "whatif_index"
	ToolDiffExcerpt     = "diff_excerpt"
	FinalTool           = "submit_review"
	Purpose             = "change_review"
)

// Limits of one review's model stage (the spec's submit_review bounds).
const (
	MaxClaims             = 4
	MaxStatements         = 10
	MaxFindings           = 8
	MaxModelNotDetermined = 6
	MaxSuggestionBytes    = 2048
	LineTolerance         = 3
	MaxExcerptBytes       = 4096
	// MaxUntouchedTables is how many relations the review does not touch
	// table_context may read (touched ones are free).
	MaxUntouchedTables = 3
	maxSeed            = 30
	maxSeedRunes       = 600
	deadlineMargin     = 1500 * time.Millisecond
)

// Bounds is one run's agentloop budget; MaxTokens is always the review's
// change_review.max_tokens_per_review.
type Bounds = agentloop.Budget

// DefaultBounds is the spec's per-review budget: 8 model steps, 16 tool
// calls, 10 probe cost units, 75 s wall clock, 3000 completion tokens and
// 30 s per step.
func DefaultBounds() Bounds {
	return Bounds{MaxSteps: 8, MaxCalls: 16, MaxCost: 10, Wall: 75 * time.Second,
		MaxTokens: 60000, StepTokens: 3000, StepTimeout: 30 * time.Second}
}

// Tools is what the closed tools read through: *impact.Engine.
type Tools interface {
	changereview.Inspector
	QueriesTouching(ctx context.Context, relation, column string) (impact.Touching, error)
	TableContext(ctx context.Context, relation string) ([]changereview.Evidence, error)
	WhatIfIndex(ctx context.Context, ddl string, statements []string) (
		changereview.Evidence, error)
}

// TokenBudget is the daily token budget every model call is reserved
// against and settled with (*changereview.Budget).
type TokenBudget interface {
	Reserve(ctx context.Context, actor string, tokens int64) (changereview.Reservation,
		error)
	Settle(ctx context.Context, r changereview.Reservation, used int64) error
}

// Deps is what the reasoner needs. Tools is required. Model nil (or a
// client that is switched off) means no model: reviews stay deterministic
// with model_status no_model. Budget is required with a model. Bounds
// zero is DefaultBounds; Protocol "" is agentloop.ProtocolAuto.
type Deps struct {
	Model    agentloop.Model
	Tools    Tools
	Budget   TokenBudget
	Bounds   Bounds
	Protocol agentloop.Protocol
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// Reasoner is the model stage; safe for concurrent reviews.
type Reasoner struct{ d Deps }

// New checks d and fills its defaults.
func New(d Deps) (*Reasoner, error) {
	if d.Tools == nil {
		return nil, fmt.Errorf("%w: the reasoner needs the impact engine's tools",
			changereview.ErrInvalid)
	}
	if isNilModel(d.Model) {
		d.Model = nil
	}
	if d.Model != nil && d.Budget == nil {
		return nil, fmt.Errorf("%w: a model needs the daily token budget",
			changereview.ErrInvalid)
	}
	if d.Bounds == (Bounds{}) {
		d.Bounds = DefaultBounds()
	}
	if err := checkBounds(d.Bounds); err != nil {
		return nil, err
	}
	switch d.Protocol {
	case "":
		d.Protocol = agentloop.ProtocolAuto
	case agentloop.ProtocolAuto, agentloop.ProtocolNative, agentloop.ProtocolJSON:
	default:
		return nil, fmt.Errorf("%w: protocol %q", changereview.ErrInvalid, d.Protocol)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return &Reasoner{d: d}, nil
}

func isNilModel(m agentloop.Model) bool {
	c, ok := m.(*llm.Client)
	return m == nil || (ok && c == nil)
}

func checkBounds(b Bounds) error {
	if b.MaxSteps < 1 || b.MaxCalls < 0 || b.MaxCost < 0 || b.Wall <= 0 ||
		b.StepTokens < 1 || b.StepTimeout <= 0 {
		return fmt.Errorf("%w: run bounds %+v", changereview.ErrInvalid, b)
	}
	return nil
}

// enabler is a model that can say it is switched off (*llm.Client).
type enabler interface{ IsEnabled() bool }

// Reason runs the model stage of one review. Provider trouble is the
// model status; the error is only the caller's cancellation.
func (r *Reasoner) Reason(ctx context.Context, in changereview.Input,
	a changereview.Analysis) (changereview.Reasoning, error) {
	if err := ctx.Err(); err != nil {
		return changereview.Reasoning{}, fmt.Errorf("model stage: %w", err)
	}
	s := r.newSession(in, a)
	if status := r.offStatus(in); status != "" {
		return s.offline(status), nil
	}
	cfg := s.config(r.bounds(ctx, in))
	model := budgetedModel{inner: r.d.Model, budget: r.d.Budget, actor: in.Actor,
		logf: r.d.Logf}
	// The run's tool calls share one workload read of the engine.
	res, err := agentloop.Run(impact.WithSessionCache(ctx), model, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return changereview.Reasoning{}, fmt.Errorf("model stage: %w", ctx.Err())
		}
		r.d.Logf("change review: review %s: model run failed: %v", in.ReviewID, err)
		return s.failed(res, err), nil
	}
	return s.conclude(res), nil
}

// offStatus is why the model does not run ("" when it does).
func (r *Reasoner) offStatus(in changereview.Input) string {
	if r.d.Model == nil {
		return changereview.ModelNoModel
	}
	if e, ok := r.d.Model.(enabler); ok && !e.IsEnabled() {
		return changereview.ModelNoModel
	}
	if in.Config.DailyTokensPerDatabase <= 0 || in.Config.DailyTokensPerActor <= 0 {
		return changereview.ModelSkipped
	}
	return ""
}

// bounds is the run's budget: the review's token cap, and a wall clock
// that ends deadlineMargin before the caller's deadline.
func (r *Reasoner) bounds(ctx context.Context, in changereview.Input) Bounds {
	b := r.d.Bounds
	if in.Config.MaxTokensPerReview > 0 {
		b.MaxTokens = in.Config.MaxTokensPerReview
	}
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - deadlineMargin
		b.Wall = max(min(b.Wall, left), 50*time.Millisecond)
	}
	return b
}

// modelStatus is the review's model status for a transcript stop.
func modelStatus(stop string) string {
	switch stop {
	case agentloop.StopFinal:
		return changereview.ModelOK
	case agentloop.StopTokens, agentloop.StopBudget:
		return changereview.ModelBudget
	case agentloop.StopRateLimited, agentloop.StopCooldown:
		return changereview.ModelRateLimited
	case agentloop.StopTimeout, agentloop.StopWall:
		return changereview.ModelTimeout
	case agentloop.StopDisabled:
		return changereview.ModelNoModel
	}
	return changereview.ModelError
}

// budgetedModel reserves each call in Change Review's daily budget before
// it is sent and settles it with the reported usage (ask's pattern). A
// refused reservation is the loop's budget stop; an unavailable budget
// store aborts the run.
type budgetedModel struct {
	inner  agentloop.Model
	budget TokenBudget
	actor  string
	logf   func(format string, args ...any)
}

func (m budgetedModel) ChatWithTools(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	est := int64(agentloop.EstimateTokens(msgs, tools, opts.MaxTokens))
	reserved, err := m.budget.Reserve(ctx, m.actor, est)
	if err != nil {
		if errors.Is(err, changereview.ErrBudgetExhausted) {
			return llm.ToolResult{}, fmt.Errorf("%w: %v", llm.ErrBudgetExhausted, err)
		}
		return llm.ToolResult{}, agentloop.Abort(fmt.Errorf("reserve %d tokens: %w", est, err))
	}
	res, err := m.inner.ChatWithTools(ctx, msgs, tools, opts)
	used := usedTokens(res, err, est)
	if serr := m.budget.Settle(context.WithoutCancel(ctx), reserved, used); serr != nil {
		m.logf("change review: settle %d of %d reserved tokens for %s: %v", used, est,
			m.actor, serr)
	}
	return res, err
}

// usedTokens is what a call is charged: the reported usage, nothing for a
// call refused before any provider I/O, else the estimate.
func usedTokens(res llm.ToolResult, err error, est int64) int64 {
	switch {
	case res.Tokens > 0:
		return int64(res.Tokens)
	case errors.Is(err, llm.ErrLLMDisabled), errors.Is(err, llm.ErrRequestCooldown),
		errors.Is(err, llm.ErrBudgetExhausted), errors.Is(err, llm.ErrInvalidToolRequest),
		errors.Is(err, llm.ErrRateLimited):
		return 0
	}
	return est
}
