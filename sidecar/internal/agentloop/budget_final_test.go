package agentloop

import (
	"context"
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
