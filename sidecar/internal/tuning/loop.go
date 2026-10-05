package tuning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// The bounded tool loop: the model may call the read-only tools for a few
// turns, then must answer. It sits behind the Loop interface so another
// loop (the W3-B investigator's) can replace it.

// Model is a tool-calling chat model (*llm.Client satisfies it).
type Model interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec,
		opts llm.ToolOptions) (llm.ToolResult, error)
}

// ToolExecutor runs one tool call and returns its result for the model.
type ToolExecutor func(ctx context.Context, call llm.ToolCall) string

// Conversation is one bounded exchange about a case.
type Conversation struct {
	System    string
	User      string
	Tools     []llm.ToolSpec
	Exec      ToolExecutor
	MaxTurns  int
	MaxTokens int
	Timeout   time.Duration
	Budget    *CycleBudget
}

// StopReason is why a conversation ended.
type StopReason string

// Stop reasons.
const (
	StopAnswered StopReason = "answered"
	StopMaxTurns StopReason = "max_turns"
	StopBudget   StopReason = "budget"
)

// Transcript is how a conversation went.
type Transcript struct {
	Final     string
	Turns     int
	ToolCalls int
	Stop      StopReason
}

// Loop runs a conversation with a model.
type Loop interface {
	Run(ctx context.Context, m Model, c Conversation) (Transcript, error)
}

// Loop errors.
var (
	// ErrBudget: the cycle's request or token budget ran out.
	ErrBudget = errors.New("tuning: cycle model budget exhausted")
	// ErrNoAnswer: the model was still calling tools on its last turn.
	ErrNoAnswer = errors.New("tuning: the model did not answer within its turns")
)

type toolLoop struct{}

// NewToolLoop returns the bounded tool loop.
func NewToolLoop() Loop { return toolLoop{} }

// Run asks the model until it answers without tool calls. Every turn
// takes a request from the budget; the last turn forbids tools, and
// MaxTurns below 1 means one answer-only turn.
func (toolLoop) Run(ctx context.Context, m Model, c Conversation) (Transcript, error) {
	if m == nil || c.Exec == nil {
		return Transcript{}, errors.New("tuning: tool loop needs a model and a tool executor")
	}
	turns := max(c.MaxTurns, 1)
	msgs := []llm.Message{{Role: "system", Content: c.System},
		{Role: "user", Content: c.User}}
	var tr Transcript
	for tr.Turns < turns {
		if err := ctx.Err(); err != nil {
			return tr, err
		}
		if c.Budget != nil && !c.Budget.TakeRequest() {
			tr.Stop = StopBudget
			return tr, fmt.Errorf("%w: request cap reached", ErrBudget)
		}
		last := tr.Turns == turns-1
		res, err := m.ChatWithTools(ctx, msgs, c.Tools, turnOptions(c, last))
		tr.Turns++
		if err != nil {
			return stopOn(tr, err)
		}
		if len(res.ToolCalls) == 0 {
			tr.Final, tr.Stop = res.Content, StopAnswered
			return tr, nil
		}
		if last {
			break
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: res.Content,
			ToolCalls: res.ToolCalls})
		for _, call := range res.ToolCalls {
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: call.ID,
				Content: c.Exec(ctx, call)})
			tr.ToolCalls++
		}
	}
	tr.Stop = StopMaxTurns
	return tr, ErrNoAnswer
}

func turnOptions(c Conversation, last bool) llm.ToolOptions {
	opts := llm.ToolOptions{MaxTokens: c.MaxTokens, Timeout: c.Timeout,
		ToolChoice: llm.ToolChoiceAuto}
	if c.Budget != nil {
		opts.Budget = c.Budget
	}
	if last {
		opts.ToolChoice = llm.ToolChoiceNone
	}
	return opts
}

// stopOn ends a conversation on a model error; a token-budget refusal is a
// budget stop that stays distinguishable as the client's error too.
func stopOn(tr Transcript, err error) (Transcript, error) {
	if errors.Is(err, llm.ErrBudgetExhausted) {
		tr.Stop = StopBudget
		return tr, fmt.Errorf("%w: %w", ErrBudget, err)
	}
	return tr, fmt.Errorf("tuning: model turn %d: %w", tr.Turns, err)
}
