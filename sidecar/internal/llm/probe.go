package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ProbeResult is the outcome of one connection test (Probe).
type ProbeResult struct {
	// Class is "ok" on an answer, an error class (errclass.go), or a local
	// refusal: disabled, circuit_open, budget, empty or error.
	Class        string `json:"class"`
	LatencyMS    int64  `json:"latency_ms"`
	Tokens       int    `json:"tokens"`
	Model        string `json:"model"`
	CircuitState string `json:"circuit_state"`
	Error        string `json:"error,omitempty"`
}

// probeMaxTokens keeps the connection test tiny.
const probeMaxTokens = 16

// probeSeq makes every probe prompt unique: two probes inside one clock
// tick (about 0.5 ms on Windows) would otherwise share a prompt and the
// second would be refused by the duplicate-prompt cooldown.
var probeSeq atomic.Uint64

// Probe sends one tiny chat request through the normal path: it is
// charged to the budgets and, when the breaker is open, admitted as the
// half-open probe even inside the cooldown, so a success closes the
// breaker. Each probe carries a nonce so the duplicate-prompt cooldown
// never suppresses a repeated test.
//
//llm:no-db-content a fixed system prompt and a nonce, with no database text
func (c *Client) Probe(ctx context.Context) ProbeResult {
	if c == nil || !c.IsEnabled() {
		return ProbeResult{Class: "disabled", CircuitState: breakerClosed.String()}
	}
	user := fmt.Sprintf("Connection test %d-%d. Reply with OK.",
		time.Now().UnixNano(), probeSeq.Add(1))
	start := time.Now()
	r, err := c.ChatResult(WithPurpose(withForcedProbe(ctx), "probe"),
		"Reply with the single word OK.",
		user, probeMaxTokens)
	res := ProbeResult{
		Class:        probeClass(err),
		LatencyMS:    time.Since(start).Milliseconds(),
		Tokens:       r.Tokens,
		Model:        c.Model(),
		CircuitState: c.breakerSnapshot().state,
	}
	if err != nil {
		res.Error = redactProviderText(err.Error())
	}
	return res
}

func probeClass(err error) string {
	if err == nil {
		return "ok"
	}
	if class := ErrorClass(err); class != "" {
		return class
	}
	switch {
	case errors.Is(err, ErrCircuitOpen):
		return "circuit_open"
	case errors.Is(err, ErrBudgetExhausted):
		return "budget"
	case errors.Is(err, ErrEmptyResponse):
		return "empty"
	}
	return "error"
}

// ProbeLLM tests the general client's connection (Probe).
func (m *Manager) ProbeLLM(ctx context.Context) ProbeResult {
	if m == nil {
		return (*Client)(nil).Probe(ctx)
	}
	return m.General.Probe(ctx)
}
