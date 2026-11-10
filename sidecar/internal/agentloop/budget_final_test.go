package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Final-only steps, pinned by mutation testing: on the last step, and
// whenever the token budget cannot pay for two more calls, only the
// final tool is offered and any other call is refused, not run.

func TestBudget_LastStepRefusesToolsItDidNotOffer(t *testing.T) {
	tool := &countingTool{}
	m := newScript(callsTools("lookup", `{"n":1}`), callsTools("lookup", `{"n":2}`))
	cfg := baseConfig(tool.tool("lookup", 0, true, "x"))
	cfg.Budget.MaxSteps = 2
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tool.count() != 1 || res.Transcript.Rejected[RejectCallBudget] != 1 {
		t.Fatalf("tool ran %d times, rejected %v; the last step may only conclude",
			tool.count(), res.Transcript.Rejected)
	}
	if got := toolNames(m.request(t, 1).tools); got != "submit_conclusion" {
		t.Fatalf("last step offered %q, want only the final", got)
	}
	if res.Transcript.Stop != StopMaxSteps {
		t.Fatalf("stop = %s", res.Transcript.Stop)
	}
}

func TestBudget_TightTokensOfferOnlyTheFinal(t *testing.T) {
	tool := &countingTool{}
	cfg := baseConfig(tool.tool("lookup", 0, true, "x"))
	cfg.Budget.MaxTokens = 100_000
	first := callsTools("lookup", `{"n":1}`)
	spend := func(call int, r request) (llm.ToolResult, error) {
		res, err := first(call, r)
		// Leave 1.6 calls' worth: the next call fits, two do not.
		est := EstimateTokens(r.msgs, r.tools, cfg.Budget.StepTokens)
		res.Tokens = cfg.Budget.MaxTokens - est*16/10
		return res, err
	}
	m := newScript(spend, callsTools("lookup", `{"n":2}`))
	m.after = callsTools("submit_conclusion", finalArgs("inconclusive"))
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolNames(m.request(t, 1).tools); got != "submit_conclusion" {
		t.Fatalf("with tokens for one more call the model was offered %q", got)
	}
	if tool.count() != 1 || res.Transcript.Rejected[RejectCallBudget] != 1 {
		t.Fatalf("tool ran %d times, rejected %v", tool.count(), res.Transcript.Rejected)
	}
}

// A thinking model's reasoning allowance (sre-04): every call reserves
// ReasoningPerCall on top of the estimate, so the loop must count it
// when it decides whether two more calls fit and whether the next one
// does. No concurrent-access case: a run is a single goroutine.

// reservingModel refuses a call as the investigator's store does: it
// reserves the estimate plus the reasoning allowance against its cap and
// charges what the reply reports.
type reservingModel struct {
	inner     Model
	cap, used int
	reasoning int
}

func (m *reservingModel) ChatWithTools(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	need := EstimateTokens(msgs, tools, opts.MaxTokens) + m.reasoning
	if m.used+need > m.cap {
		return llm.ToolResult{}, fmt.Errorf("%w: investigator tokens %d held of %d",
			llm.ErrBudgetExhausted, m.used, m.cap)
	}
	res, err := m.inner.ChatWithTools(ctx, msgs, tools, opts)
	m.used += res.Tokens
	return res, err
}

// byOffer concludes when only the final tool is offered and otherwise
// asks for a lookup, reporting the estimate without any reasoning.
func byOffer(step int) reply {
	return func(_ int, r request) (llm.ToolResult, error) {
		call := callsTools("lookup", `{"n":1}`)
		if toolNames(r.tools) == "submit_conclusion" {
			call = callsTools("submit_conclusion", finalArgs("inconclusive"))
		}
		res, err := call(0, r)
		res.Tokens = EstimateTokens(r.msgs, r.tools, step)
		return res, err
	}
}

func TestBudget_ReasoningAllowanceMakesTheLastAffordableCallFinal(t *testing.T) {
	tool := &countingTool{}
	cfg := baseConfig(tool.tool("lookup", 0, true, "x"))
	cfg.Budget.StepTokens, cfg.Budget.ReasoningPerCall = 1024, 2048
	full := EstimateTokens(firstMessages(cfg, ProtocolNative), offered(cfg, false),
		cfg.Budget.StepTokens)
	// Two calls fit without reasoning, not with it: after one lookup the
	// remaining budget is one estimate + 1000, short of the reservation.
	cfg.Budget.MaxTokens = 2*full + 1000
	script := newScript()
	script.after = byOffer(cfg.Budget.StepTokens)
	m := &reservingModel{inner: script, cap: cfg.Budget.MaxTokens, reasoning: 2048}
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopFinal || res.Final == nil {
		t.Fatalf("stop = %s (%s), want final: the reasoning allowance was not counted",
			res.Transcript.Stop, res.Transcript.StopDetail)
	}
	if got := toolNames(script.request(t, 0).tools); got != "submit_conclusion" ||
		script.calls() != 1 || tool.count() != 0 {
		t.Fatalf("offered %q in %d calls, tool ran %d times; want one final-only call",
			got, script.calls(), tool.count())
	}
	if opts := script.request(t, 0).opts; opts.ToolChoice != llm.ToolChoiceRequired {
		t.Fatalf("tool choice = %q, want required", opts.ToolChoice)
	}
}

func TestBudget_ReasoningPerCallShiftsTheTokenBoundaryExactly(t *testing.T) {
	for _, reasoning := range []int{0, 1, 2048} {
		cfg := baseConfig()
		cfg.Budget.ReasoningPerCall = reasoning
		first := firstMessages(cfg, ProtocolNative)
		// One call left: only the final is offered, at estimate + reasoning.
		need := EstimateTokens(first, offered(cfg, true), cfg.Budget.StepTokens) + reasoning
		for _, tc := range []struct {
			budget, calls int
			stop          string
		}{{need, 1, StopFinal}, {need - 1, 0, StopTokens}} {
			m := newScript(callsTools("submit_conclusion", finalArgs("inconclusive")))
			c := cfg
			c.Budget.MaxTokens = tc.budget
			res, err := Run(context.Background(), m, c)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if m.calls() != tc.calls || res.Transcript.Stop != tc.stop {
				t.Fatalf("reasoning %d, budget %d (need %d): %d calls, stop %s; want %d, %s",
					reasoning, tc.budget, need, m.calls(), res.Transcript.Stop, tc.calls,
					tc.stop)
			}
		}
	}
}

func TestBudget_TokenStopNamesTheReasoningInTheEstimate(t *testing.T) {
	cfg := baseConfig()
	cfg.Budget.ReasoningPerCall = 2048
	need := EstimateTokens(firstMessages(cfg, ProtocolNative), offered(cfg, true),
		cfg.Budget.StepTokens) + 2048
	cfg.Budget.MaxTokens = need - 1
	m := newScript()
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := fmt.Sprintf("needs about %d of the %d budget", need, need-1)
	if res.Transcript.Stop != StopTokens || !strings.Contains(res.Transcript.StopDetail,
		want) || m.calls() != 0 {
		t.Fatalf("stop %s %q after %d calls, want token_budget naming %q",
			res.Transcript.Stop, res.Transcript.StopDetail, m.calls(), want)
	}
}

func TestBudget_UnreportedTokensChargeTheReasoningAllowance(t *testing.T) {
	for _, reasoning := range []int{0, 2048} {
		cfg := baseConfig()
		cfg.Budget.ReasoningPerCall = reasoning
		est := EstimateTokens(firstMessages(cfg, ProtocolNative), offered(cfg, false),
			cfg.Budget.StepTokens)
		m := newScript(func(int, request) (llm.ToolResult, error) {
			return llm.ToolResult{ToolCalls: []llm.ToolCall{{ID: "c",
				Name:      "submit_conclusion",
				Arguments: json.RawMessage(finalArgs("inconclusive"))}}}, nil
		})
		res, err := Run(context.Background(), m, cfg)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Transcript.Tokens != est+reasoning {
			t.Fatalf("reasoning %d: tokens = %d, want the reservation %d", reasoning,
				res.Transcript.Tokens, est+reasoning)
		}
	}
}

func TestBudget_NegativeReasoningIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		reasoning int
		valid     bool
	}{{-1, false}, {-2048, false}, {0, true}, {1, true}} {
		cfg := baseConfig()
		cfg.Budget.ReasoningPerCall = tc.reasoning
		err := cfg.Validate()
		if tc.valid != (err == nil) {
			t.Fatalf("reasoning %d: Validate = %v, valid want %v", tc.reasoning, err,
				tc.valid)
		}
		if !tc.valid && (!errors.Is(err, ErrInvalidConfig) ||
			!strings.Contains(err.Error(), "reasoning")) {
			t.Fatalf("reasoning %d: %v, want ErrInvalidConfig naming the reasoning",
				tc.reasoning, err)
		}
	}
	m := newScript()
	cfg := baseConfig()
	cfg.Budget.ReasoningPerCall = -1
	if _, err := Run(context.Background(), m, cfg); !errors.Is(err, ErrInvalidConfig) ||
		m.calls() != 0 {
		t.Fatalf("Run with negative reasoning: %v after %d calls", err, m.calls())
	}
}
