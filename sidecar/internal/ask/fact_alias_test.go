package ask

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
)

// mcp-02: the loop tells the model to cite evidence only by its aliases
// (E1, E2, ...), so propose_fact takes them, resolved through the run,
// and still refuses evidence this session did not read. These tests need
// no database: every case is decided before the fact store is reached.

// stepModel answers each model call with the next scripted tool calls.
type stepModel struct {
	mu    sync.Mutex
	steps [][]llm.ToolCall
	n     int
}

func (m *stepModel) ChatWithTools(_ context.Context, _ []llm.Message, _ []llm.ToolSpec,
	_ llm.ToolOptions) (llm.ToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n >= len(m.steps) {
		return llm.ToolResult{}, errors.New("script exhausted")
	}
	calls := m.steps[m.n]
	m.n++
	return llm.ToolResult{ToolCalls: calls, Tokens: 10}, nil
}

func step(name, args string) []llm.ToolCall {
	return []llm.ToolCall{{ID: "c_" + name, Name: name, Arguments: json.RawMessage(args)}}
}

// citationResult is what one factCitations call inside the run returned.
type citationResult struct {
	refs  []string
	cites int
	first string
	err   error
}

// runCitations runs a loop whose tools are: read (the session reads
// finding:5, E1), foreign (evidence the loop holds but the session never
// read, E2) and cite (factCitations over each ref list, in turn).
func runCitations(t *testing.T, ss *session, refLists ...[]string) []citationResult {
	t.Helper()
	var out []citationResult
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)
	read := agentloop.Tool{Name: "read", Description: "read", Parameters: schema,
		Run: func(context.Context, json.RawMessage) (agentloop.Output, error) {
			return ss.cite("finding:5", "open", "finding 5: missing index"), nil
		}}
	foreign := agentloop.Tool{Name: "foreign", Description: "foreign", Parameters: schema,
		Run: func(context.Context, json.RawMessage) (agentloop.Output, error) {
			return agentloop.Output{Status: "ok", Text: "x", Evidence: &agentloop.Evidence{
				ID: "finding:777", Digest: "dd", Label: "finding:777 open", Text: "x"}}, nil
		}}
	n := 0
	cite := agentloop.Tool{Name: "cite", Description: "cite", Parameters: schema,
		Run: func(ctx context.Context, _ json.RawMessage) (agentloop.Output, error) {
			refs := refLists[n]
			n++
			cites, err := ss.factCitations(ctx, refs)
			r := citationResult{refs: refs, cites: len(cites), err: err}
			if len(cites) > 0 {
				r.first = cites[0].Ref + "|" + cites[0].Kind + "|" + cites[0].Detail
			}
			out = append(out, r)
			return agentloop.Output{Status: "ok", Text: "cite ran"}, nil
		}}
	m := &stepModel{steps: [][]llm.ToolCall{step("read", `{}`), step("foreign", `{}`)}}
	for i := range refLists {
		m.steps = append(m.steps, step("cite", `{"n":`+itoa(int64(i))+`}`))
	}
	m.steps = append(m.steps, step(FinalTool, `{"claims":[]}`))
	cfg := agentloop.Config{Task: "q", Tools: []agentloop.Tool{read, foreign, cite},
		Final: finalTool(), Protocol: agentloop.ProtocolNative,
		Budget: agentloop.Budget{MaxSteps: 12, MaxCalls: 12, MaxCost: 12,
			Wall: 10 * time.Second, MaxTokens: 100_000, StepTokens: 100,
			StepTimeout: 5 * time.Second}}
	if _, err := agentloop.Run(context.Background(), m, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != len(refLists) {
		t.Fatalf("cite ran %d times, want %d", len(out), len(refLists))
	}
	return out
}

func TestFactCitations_AliasesResolveThroughTheRun(t *testing.T) {
	ss := (&Service{}).newSession(operator)
	digest := digestOf("finding 5: missing index")
	want := "finding:5|ask_sage:finding|finding:5 open sha256:" + digest
	got := runCitations(t, ss, []string{"E1"}, []string{"E1", "finding:5"},
		[]string{"E2"}, []string{"E9"}, []string{"e1"}, []string{"E1", "E2"})
	// E1 and finding:5 name the same evidence, so it is cited once.
	for i, r := range got[:2] {
		if r.err != nil || r.first != want || r.cites != 1 {
			t.Errorf("case %d %v: %d cites, first %q, err %v; want 1 cite, first %q",
				i, r.refs, r.cites, r.first, r.err, want)
		}
	}
	// E2 is the loop's alias for evidence the session never read; E9 was
	// never issued; aliases are exact; one unread ref refuses the list.
	for _, r := range got[2:] {
		if !errors.Is(r.err, agentloop.ErrInvalidArgs) || r.cites != 0 {
			t.Errorf("%v: %d cites, err %v; want ErrInvalidArgs", r.refs, r.cites, r.err)
		}
	}
	if !strings.Contains(got[3].err.Error(), `"E9"`) {
		t.Errorf("refusal %q does not name the unread ref", got[3].err)
	}
}

func TestFactCitations_CountBoundaries(t *testing.T) {
	ss := (&Service{}).newSession(operator)
	var ids []string
	for i := 1; i <= maxFactEvidence+1; i++ {
		id := "finding:" + itoa(int64(i))
		ss.cite(id, "open", "finding "+id)
		ids = append(ids, id)
	}
	ctx := context.Background()
	cites, err := ss.factCitations(ctx, ids[:maxFactEvidence])
	if err != nil || len(cites) != maxFactEvidence ||
		cites[maxFactEvidence-1].Ref != ids[maxFactEvidence-1] {
		t.Fatalf("%d refs: %d cites (%v), want %d", maxFactEvidence, len(cites), err,
			maxFactEvidence)
	}
	same := []string{ids[0], ids[1], ids[0], ids[0], ids[1], ids[0]}
	if cites, err := ss.factCitations(ctx, same); err != nil || len(cites) != 2 ||
		cites[0].Ref != ids[0] || cites[1].Ref != ids[1] {
		t.Fatalf("repeated refs: %+v (%v), want %s then %s once each", cites, err, ids[0],
			ids[1])
	}
	for _, refs := range [][]string{nil, {}, ids} {
		if cites, err := ss.factCitations(ctx, refs); !errors.Is(err,
			agentloop.ErrInvalidArgs) || cites != nil {
			t.Errorf("%d refs: cites %+v err %v, want ErrInvalidArgs", len(refs), cites, err)
		}
	}
}

// Without a run nothing is an alias: only durable ids the session read.
func TestFactCitations_NoRunTakesOnlyDurableIDs(t *testing.T) {
	ss := (&Service{}).newSession(operator)
	ss.cite("finding:5", "open", "finding 5")
	if _, err := ss.factCitations(context.Background(), []string{"E1"}); !errors.Is(err,
		agentloop.ErrInvalidArgs) {
		t.Fatalf("E1 outside a run: err %v, want ErrInvalidArgs", err)
	}
	cites, err := ss.factCitations(context.Background(), []string{"finding:5"})
	if err != nil || len(cites) != 1 || cites[0].Ref != "finding:5" {
		t.Fatalf("durable id outside a run: %+v (%v)", cites, err)
	}
}
