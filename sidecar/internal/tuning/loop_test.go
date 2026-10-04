package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// The bounded tool loop. It sits behind the Loop interface so the W3-B
// investigator's loop can replace it.

func conv(budget *CycleBudget, exec ToolExecutor) Conversation {
	return Conversation{System: "sys", User: "case", MaxTurns: 3, MaxTokens: 512,
		Timeout: 7 * time.Second, Budget: budget, Exec: exec,
		Tools: []llm.ToolSpec{{Name: "statement", Description: "d",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}}
}

func echoExec(calls *[]llm.ToolCall) ToolExecutor {
	return func(_ context.Context, c llm.ToolCall) string {
		*calls = append(*calls, c)
		return `{"evidence_id":"R1","calls":600}`
	}
}

func TestToolLoop_DirectAnswer(t *testing.T) {
	m := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		answer(`{"proposals":[]}`)}}
	var calls []llm.ToolCall
	tr, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(5, 1e6),
		echoExec(&calls)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if tr.Final != `{"proposals":[]}` || tr.Turns != 1 || tr.ToolCalls != 0 ||
		tr.Stop != StopAnswered || len(calls) != 0 {
		t.Fatalf("transcript = %+v", tr)
	}
	msgs := m.msgs[0]
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != "sys" ||
		msgs[1].Role != "user" || msgs[1].Content != "case" {
		t.Fatalf("first turn messages = %+v", msgs)
	}
}

func TestToolLoop_ToolCallThenAnswer(t *testing.T) {
	m := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		toolCall("statement", `{"queryid":101}`), answer("done")}}
	var calls []llm.ToolCall
	tr, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(5, 1e6),
		echoExec(&calls)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if tr.Final != "done" || tr.Turns != 2 || tr.ToolCalls != 1 {
		t.Fatalf("transcript = %+v", tr)
	}
	if len(calls) != 1 || calls[0].Name != "statement" ||
		string(calls[0].Arguments) != `{"queryid":101}` {
		t.Fatalf("executed = %+v", calls)
	}
	second := m.msgs[1]
	if len(second) != 4 {
		t.Fatalf("second turn has %d messages, want system, user, assistant, tool",
			len(second))
	}
	if second[2].Role != "assistant" || len(second[2].ToolCalls) != 1 {
		t.Fatalf("assistant turn = %+v", second[2])
	}
	if second[3].Role != "tool" || second[3].ToolCallID != "call_statement" ||
		!strings.Contains(second[3].Content, "R1") {
		t.Fatalf("tool result = %+v", second[3])
	}
}

func TestToolLoop_OptionsCarryTheBudgetAndBounds(t *testing.T) {
	m := &scriptedModel{}
	budget := NewCycleBudget(5, 1e6)
	var calls []llm.ToolCall
	if _, err := NewToolLoop().Run(context.Background(), m, conv(budget,
		echoExec(&calls))); err != nil {
		t.Fatalf("run: %v", err)
	}
	opts := m.opts[0]
	if opts.Budget != budget || opts.MaxTokens != 512 || opts.Timeout != 7*time.Second {
		t.Fatalf("options = %+v", opts)
	}
}

func TestToolLoop_LastTurnForbidsTools(t *testing.T) {
	m := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		toolCall("statement", `{}`), toolCall("statement", `{}`), answer("final")}}
	var calls []llm.ToolCall
	tr, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(5, 1e6),
		echoExec(&calls)))
	if err != nil || tr.Final != "final" || tr.Turns != 3 {
		t.Fatalf("transcript %+v err %v", tr, err)
	}
	if m.opts[0].ToolChoice == llm.ToolChoiceNone || m.opts[2].ToolChoice != llm.ToolChoiceNone {
		t.Fatalf("tool choice per turn = %q %q %q", m.opts[0].ToolChoice,
			m.opts[1].ToolChoice, m.opts[2].ToolChoice)
	}
}

func TestToolLoop_NeverAnswering(t *testing.T) {
	m := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		toolCall("statement", `{}`), toolCall("statement", `{}`), toolCall("statement", `{}`)}}
	var calls []llm.ToolCall
	tr, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(5, 1e6),
		echoExec(&calls)))
	if !errors.Is(err, ErrNoAnswer) || tr.Stop != StopMaxTurns || tr.Turns != 3 {
		t.Fatalf("transcript %+v err %v", tr, err)
	}
	if m.callCount() != 3 {
		t.Fatalf("model called %d times, want the turn cap 3", m.callCount())
	}
}

func TestToolLoop_RequestBudgetStopsTheConversation(t *testing.T) {
	m := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		toolCall("statement", `{}`), answer("never reached")}}
	var calls []llm.ToolCall
	tr, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(1, 1e6),
		echoExec(&calls)))
	if !errors.Is(err, ErrBudget) || tr.Stop != StopBudget {
		t.Fatalf("transcript %+v err %v", tr, err)
	}
	if m.callCount() != 1 {
		t.Fatalf("model called %d times, want 1", m.callCount())
	}
}

func TestToolLoop_TokenBudgetRefusalIsABudgetStop(t *testing.T) {
	m := &scriptedModel{}
	var calls []llm.ToolCall
	tr, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(5, 50),
		echoExec(&calls)))
	if !errors.Is(err, ErrBudget) || !errors.Is(err, llm.ErrBudgetExhausted) ||
		tr.Stop != StopBudget {
		t.Fatalf("transcript %+v err %v", tr, err)
	}
}

func TestToolLoop_ModelErrorsPropagate(t *testing.T) {
	m := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		failing(llm.ErrRateLimited)}}
	var calls []llm.ToolCall
	_, err := NewToolLoop().Run(context.Background(), m, conv(NewCycleBudget(5, 1e6),
		echoExec(&calls)))
	if !errors.Is(err, llm.ErrRateLimited) {
		t.Fatalf("err = %v, want the rate limit to stay distinguishable", err)
	}
}

func TestToolLoop_InvalidInput(t *testing.T) {
	var calls []llm.ToolCall
	if _, err := NewToolLoop().Run(context.Background(), nil, conv(NewCycleBudget(5, 1e6),
		echoExec(&calls))); err == nil {
		t.Fatal("a nil model is an error")
	}
	c := conv(NewCycleBudget(5, 1e6), nil)
	if _, err := NewToolLoop().Run(context.Background(), &scriptedModel{}, c); err == nil {
		t.Fatal("a nil tool executor is an error")
	}
	c = conv(NewCycleBudget(5, 1e6), echoExec(&calls))
	c.MaxTurns = 0
	m := &scriptedModel{}
	tr, err := NewToolLoop().Run(context.Background(), m, c)
	if err != nil || tr.Turns != 1 || m.opts[0].ToolChoice != llm.ToolChoiceNone {
		t.Fatalf("MaxTurns 0 means one answer-only turn: %+v %v", tr, err)
	}
}

func TestToolLoop_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &scriptedModel{}
	var calls []llm.ToolCall
	_, err := NewToolLoop().Run(ctx, m, conv(NewCycleBudget(5, 1e6), echoExec(&calls)))
	if !errors.Is(err, context.Canceled) || m.callCount() != 0 {
		t.Fatalf("err %v calls %d: a cancelled context calls no model", err, m.callCount())
	}
}
