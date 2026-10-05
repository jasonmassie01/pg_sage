package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Budget stops: every run ends within its steps, tool calls, probe cost,
// wall clock and tokens, whatever the model does.

func TestBudget_ToolSpamIsCappedByCalls(t *testing.T) {
	tool := &countingTool{}
	spam := []string{}
	for i := 0; i < 8; i++ {
		spam = append(spam, "lookup", fmt.Sprintf(`{"n":%d}`, i))
	}
	m := newScript(callsTools(spam...), callsTools(spam...))
	m.after = callsTools("submit_conclusion", finalArgs("inconclusive"))
	cfg := baseConfig(tool.tool("lookup", 0, true, "x"))
	cfg.Budget.MaxCalls = 3
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := res.Transcript
	if tool.count() != 3 || tr.Rejected[RejectCallBudget] != 13 {
		t.Fatalf("tool ran %d times, rejected %v; want 3 runs and 13 call_budget",
			tool.count(), tr.Rejected)
	}
	if got := toolNames(m.request(t, 2).tools); got != "submit_conclusion" {
		t.Fatalf("after the call budget the model was offered %q, want only the final", got)
	}
}

func TestBudget_CostCapsChargedToolsButNotFreeOnes(t *testing.T) {
	probe, free := &countingTool{}, &countingTool{}
	m := newScript(callsTools("probe", `{"n":1}`, "probe", `{"n":2}`, "graph", `{}`),
		callsTools("submit_conclusion", finalArgs("inconclusive")))
	cfg := baseConfig(probe.tool("probe", 1, true, "x"), free.tool("graph", 0, false, "y"))
	cfg.Budget.MaxCost = 1
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.count() != 1 || free.count() != 1 || res.Transcript.Cost != 1 ||
		res.Transcript.Rejected[RejectCostBudget] != 1 {
		t.Fatalf("probe %d free %d transcript %+v", probe.count(), free.count(),
			res.Transcript)
	}
}

func TestBudget_CostExactlyAtTheCapRuns(t *testing.T) {
	probe := &countingTool{}
	m := newScript(callsTools("probe", `{"n":1}`, "probe", `{"n":2}`),
		callsTools("submit_conclusion", finalArgs("inconclusive")))
	cfg := baseConfig(probe.tool("probe", 1, true, "x"))
	cfg.Budget.MaxCost = 2
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.count() != 2 || res.Transcript.Rejected[RejectCostBudget] != 0 {
		t.Fatalf("probe ran %d times, rejected %v; the second call fits the cap exactly",
			probe.count(), res.Transcript.Rejected)
	}
}

func TestBudget_DuplicateCallReusesTheFirstResult(t *testing.T) {
	probe := &countingTool{}
	m := newScript(callsTools("probe", `{"n":1}`),
		callsTools("probe", `{ "n" : 1 }`),
		callsTools("submit_conclusion", finalArgs("inconclusive")))
	res, err := Run(context.Background(), m, baseConfig(probe.tool("probe", 1, true, "x")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.count() != 1 || res.Transcript.Rejected[RejectDuplicate] != 1 ||
		res.Transcript.Cost != 1 {
		t.Fatalf("probe ran %d times, transcript %+v", probe.count(), res.Transcript)
	}
	if !strings.Contains(lastUserText(m.request(t, 2)), "E2") {
		t.Fatal("the duplicate's answer does not point at the first result's alias")
	}
}

func TestBudget_ALoopThatNeverConcludesStopsAtMaxSteps(t *testing.T) {
	free := &countingTool{}
	m := newScript()
	n := 0
	m.after = func(int, request) (llm.ToolResult, error) {
		n++
		return callsTools("graph", fmt.Sprintf(`{"n":%d}`, n))(0, request{})
	}
	cfg := baseConfig(free.tool("graph", 0, false, "y"))
	cfg.Budget.MaxSteps = 3
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopMaxSteps || m.calls() != 3 || res.Final != nil {
		t.Fatalf("stop %s after %d calls, final %s; want max_steps after 3 and none",
			res.Transcript.Stop, m.calls(), res.Final)
	}
	if got := toolNames(m.request(t, 2).tools); got != "submit_conclusion" {
		t.Fatalf("the last step offered %q, want only the final tool", got)
	}
	if !strings.Contains(lastUserText(m.request(t, 2)), "submit_conclusion") {
		t.Fatal("the last step does not ask for the conclusion")
	}
}

func TestBudget_WallClockStopsBeforeTheNextCall(t *testing.T) {
	clock := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	free := &countingTool{}
	m := newScript()
	m.after = func(call int, r request) (llm.ToolResult, error) {
		mu.Lock()
		clock = clock.Add(4 * time.Second)
		mu.Unlock()
		return callsTools("graph", fmt.Sprintf(`{"n":%d}`, call))(call, r)
	}
	cfg := baseConfig(free.tool("graph", 0, false, "y"))
	cfg.Budget.Wall = 10 * time.Second
	cfg.Budget.MaxSteps = 10
	cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopWall || m.calls() != 3 {
		t.Fatalf("stop %s after %d calls; at 4 s a call, the 4th starts at 12 s > 10 s",
			res.Transcript.Stop, m.calls())
	}
}

func TestBudget_StepTimeoutBoundsEachCall(t *testing.T) {
	m := newScript(func(_ int, r request) (llm.ToolResult, error) {
		return llm.ToolResult{}, fmt.Errorf("provider: %w", context.DeadlineExceeded)
	})
	cfg := baseConfig()
	cfg.Budget.StepTimeout = 50 * time.Millisecond
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopTimeout {
		t.Fatalf("stop = %s, want timeout", res.Transcript.Stop)
	}
	if got := m.request(t, 0).opts.Timeout; got <= 0 || got > 50*time.Millisecond {
		t.Fatalf("call timeout = %s, want within the 50ms step timeout", got)
	}
}

func TestBudget_TokenCapStopsBeforeAnOversizedCall(t *testing.T) {
	free := &countingTool{}
	m := newScript(func(int, request) (llm.ToolResult, error) {
		return llm.ToolResult{ToolCalls: []llm.ToolCall{{ID: "c", Name: "graph",
			Arguments: json.RawMessage(`{}`)}}, Tokens: 900}, nil
	})
	m.after = callsTools("submit_conclusion", finalArgs("inconclusive"))
	cfg := baseConfig(free.tool("graph", 0, false, "y"))
	cfg.Budget.MaxTokens = 1000
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopTokens || m.calls() != 1 || res.Transcript.Tokens != 900 {
		t.Fatalf("stop %s after %d calls with %d tokens; want token_budget after 1",
			res.Transcript.Stop, m.calls(), res.Transcript.Tokens)
	}
}

func TestBudget_TokenCapBoundaryIsInclusive(t *testing.T) {
	cfg := baseConfig()
	first := firstMessages(cfg, ProtocolNative)
	need := EstimateTokens(first, offered(cfg, false), cfg.Budget.StepTokens)
	for _, tc := range []struct {
		budget int
		calls  int
	}{{need, 1}, {need - 1, 0}} {
		m := newScript(callsTools("submit_conclusion", finalArgs("inconclusive")))
		c := baseConfig()
		c.Budget.MaxTokens = tc.budget
		res, err := Run(context.Background(), m, c)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if m.calls() != tc.calls {
			t.Fatalf("budget %d (estimate %d): %d calls, want %d (stop %s)", tc.budget,
				need, m.calls(), tc.calls, res.Transcript.Stop)
		}
	}
}

func TestBudget_ReportedTokensFallBackToTheEstimate(t *testing.T) {
	m := newScript(func(int, request) (llm.ToolResult, error) {
		return llm.ToolResult{ToolCalls: []llm.ToolCall{{ID: "c",
				Name: "submit_conclusion", Arguments: json.RawMessage(finalArgs("inconclusive"))}}},
			nil
	})
	res, err := Run(context.Background(), m, baseConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Tokens <= 0 {
		t.Fatalf("tokens = %d; an unreported usage must count the estimate",
			res.Transcript.Tokens)
	}
}

func TestBudget_ProviderStopsAreTyped(t *testing.T) {
	cases := map[string]error{
		StopRateLimited: fmt.Errorf("%w (status 429)", llm.ErrRateLimited),
		StopBudget:      fmt.Errorf("%w: daily", llm.ErrBudgetExhausted),
		StopDisabled:    llm.ErrLLMDisabled,
		StopCooldown:    llm.ErrRequestCooldown,
		StopProvider:    errors.New("LLM API error 500"),
	}
	for want, cause := range cases {
		t.Run(want, func(t *testing.T) {
			m := newScript(fails(cause))
			res, err := Run(context.Background(), m, baseConfig())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Transcript.Stop != want || m.calls() != 1 || res.Final != nil {
				t.Fatalf("stop %s after %d calls; want %s after 1", res.Transcript.Stop,
					m.calls(), want)
			}
			if res.Transcript.StopDetail == "" {
				t.Fatal("stop detail is empty")
			}
		})
	}
}

func TestBudget_AutoProtocolFallsBackToJSONActionsOnce(t *testing.T) {
	tool := &countingTool{}
	m := newScript(fails(errors.New("LLM API error 400: tools are not supported")),
		says(`{"tool":"lookup","args":{"n":1}}`),
		says("```json\n{\"tool\":\"submit_conclusion\",\"args\":"+finalArgs("agree",
			Claim{Text: "lookup ok.", EvidenceIDs: []string{"E2"}})+"}\n```"))
	cfg := baseConfig(tool.tool("lookup", 1, true, "lookup ok"))
	cfg.Protocol = ProtocolAuto
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Protocol != ProtocolJSON || res.Transcript.Stop != StopFinal ||
		len(res.Claims) != 1 || tool.count() != 1 {
		t.Fatalf("transcript %+v claims %+v", res.Transcript, res.Claims)
	}
	if n := len(m.request(t, 1).tools); n != 0 {
		t.Fatalf("JSON protocol request offered %d native tools", n)
	}
	if !strings.Contains(m.request(t, 1).msgs[0].Content, `"tool"`) {
		t.Fatal("JSON protocol system prompt does not describe the action object")
	}
}

func TestBudget_AutoProtocolDoesNotFallBackAfterNativeWorked(t *testing.T) {
	tool := &countingTool{}
	m := newScript(callsTools("lookup", `{}`), fails(errors.New("LLM API error 500")))
	cfg := baseConfig(tool.tool("lookup", 1, true, "x"))
	cfg.Protocol = ProtocolAuto
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopProvider || res.Transcript.Protocol != ProtocolNative {
		t.Fatalf("transcript = %+v", res.Transcript)
	}
}

func TestBudget_TwoRunsShareOnlyTheirTools(t *testing.T) {
	shared := &countingTool{gate: make(chan struct{}, 1), hold: 20 * time.Millisecond}
	tool := shared.tool("probe", 1, true, "x")
	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := newScript(callsTools("probe", `{"n":1}`, "probe", `{"n":2}`),
				callsTools("submit_conclusion", finalArgs("inconclusive")))
			results[i], errs[i] = Run(context.Background(), m, baseConfig(tool))
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || results[i].Transcript.ToolCalls != 2 ||
			results[i].Transcript.Cost != 2 {
			t.Fatalf("run %d: %v %+v", i, errs[i], results[i].Transcript)
		}
	}
	if shared.count() != 4 || shared.maxIn != 1 {
		t.Fatalf("shared tool ran %d times, %d at once; want 4 runs, at most 1 at once",
			shared.count(), shared.maxIn)
	}
}
