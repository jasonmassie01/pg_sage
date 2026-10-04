package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Fixtures for the tool loop. scriptModel answers each model call from a
// script and records what it was sent; countingTool counts its runs and
// the most runs in flight at once. Nothing here reaches a provider.

type request struct {
	msgs  []llm.Message
	tools []llm.ToolSpec
	opts  llm.ToolOptions
}

type reply func(call int, r request) (llm.ToolResult, error)

type scriptModel struct {
	mu       sync.Mutex
	script   []reply
	requests []request
	// after answers every call past the script; nil fails the call.
	after reply
}

func newScript(script ...reply) *scriptModel { return &scriptModel{script: script} }

func (m *scriptModel) ChatWithTools(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	m.mu.Lock()
	n := len(m.requests)
	r := request{msgs: append([]llm.Message(nil), msgs...),
		tools: append([]llm.ToolSpec(nil), tools...), opts: opts}
	m.requests = append(m.requests, r)
	var next reply
	if n < len(m.script) {
		next = m.script[n]
	} else {
		next = m.after
	}
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return llm.ToolResult{}, err
	}
	if next == nil {
		return llm.ToolResult{}, errors.New("script exhausted")
	}
	return next(n, r)
}

func (m *scriptModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *scriptModel) request(t *testing.T, i int) request {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.requests) {
		t.Fatalf("request %d not made (%d made)", i, len(m.requests))
	}
	return m.requests[i]
}

// callsTools replies with native tool calls (name, JSON args) pairs.
func callsTools(pairs ...string) reply {
	return func(int, request) (llm.ToolResult, error) {
		var out []llm.ToolCall
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, llm.ToolCall{ID: "call_" + pairs[i] + itoa(i),
				Name: pairs[i], Arguments: json.RawMessage(pairs[i+1])})
		}
		return llm.ToolResult{ToolCalls: out, Tokens: 100}, nil
	}
}

// says replies with plain content (JSON actions or prose).
func says(content string) reply {
	return func(int, request) (llm.ToolResult, error) {
		return llm.ToolResult{Content: content, Tokens: 100}, nil
	}
}

func fails(err error) reply {
	return func(int, request) (llm.ToolResult, error) { return llm.ToolResult{}, err }
}

func itoa(i int) string { return strconv.Itoa(i) }

// countingTool is a tool whose runs are counted; cost is its cost and
// text what it answers. Its result is citable evidence when cite is set.
type countingTool struct {
	mu       sync.Mutex
	runs     int
	inFlight int
	maxIn    int
	gate     chan struct{} // nil: no limiter
	hold     time.Duration
	args     []string
}

func (c *countingTool) tool(name string, cost int, cite bool, text string) Tool {
	return Tool{Name: name, Description: "test tool " + name, Cost: cost,
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Run: func(ctx context.Context, args json.RawMessage) (Output, error) {
			if c.gate != nil {
				c.gate <- struct{}{}
				defer func() { <-c.gate }()
			}
			c.mu.Lock()
			c.runs++
			n := c.runs
			c.inFlight++
			c.maxIn = max(c.maxIn, c.inFlight)
			c.args = append(c.args, string(args))
			c.mu.Unlock()
			time.Sleep(c.hold)
			c.mu.Lock()
			c.inFlight--
			c.mu.Unlock()
			out := Output{Status: "ok", Text: text}
			if cite {
				out.Evidence = &Evidence{ID: name + "-ev-" + itoa(n), Digest: "d" + itoa(n),
					Label: name + " ok", Text: text}
			}
			return out, nil
		}}
}

func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

const finalSchema = `{"type":"object","properties":{"outcome":{"type":"string"},` +
	`"claims":{"type":"array"}},"required":["outcome","claims"]}`

func testFinal() Final {
	return Final{Name: "submit_conclusion", Description: "Submit the conclusion.",
		Parameters: json.RawMessage(finalSchema)}
}

func seed() []Evidence {
	return []Evidence{{ID: "seed-1", Digest: "s1", Label: "lock_graph ok",
		Text: "lock_graph ok: pid 4242 blocks 2 sessions for 90 s"}}
}

func baseConfig(tools ...Tool) Config {
	return Config{System: "You investigate.", Task: "Find the cause.", Tools: tools,
		Final: testFinal(), Seed: seed(), Protocol: ProtocolNative,
		Budget: Budget{MaxSteps: 6, MaxCalls: 8, MaxCost: 4, Wall: 10 * time.Second,
			MaxTokens: 1_000_000, StepTokens: 500, StepTimeout: 5 * time.Second}}
}

func finalArgs(outcome string, claims ...Claim) string {
	if claims == nil {
		claims = []Claim{}
	}
	raw, _ := json.Marshal(map[string]any{"outcome": outcome, "claims": claims})
	return string(raw)
}

func toolNames(specs []llm.ToolSpec) string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

func lastUserText(r request) string {
	for i := len(r.msgs) - 1; i >= 0; i-- {
		if r.msgs[i].Role == "user" || r.msgs[i].Role == "tool" {
			return r.msgs[i].Content
		}
	}
	return ""
}

func allText(r request) string {
	var b strings.Builder
	for _, m := range r.msgs {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}
	return b.String()
}
