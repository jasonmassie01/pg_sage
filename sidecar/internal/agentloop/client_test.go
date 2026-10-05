package agentloop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// The loop against the real LLM client and a fake OpenAI-compatible
// server: malformed JSON, ```json fences, empty replies, 429, timeouts,
// undeclared tools and tool-call loops that never conclude.

type openAIFake struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	script []func(w http.ResponseWriter, body string)
}

func newOpenAIFake(t *testing.T, script ...func(w http.ResponseWriter, body string)) *openAIFake {
	t.Helper()
	f := &openAIFake{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.bodies)
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		if n >= len(f.script) {
			n = len(f.script) - 1
		}
		f.script[n](w, string(raw))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *openAIFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *openAIFake) body(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func (f *openAIFake) client() *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: f.srv.URL, APIKey: "k",
		Model: "fake-model", TimeoutSeconds: 5, TokenBudgetDaily: 10_000_000},
		func(string, string, ...any) {})
}

func completion(w http.ResponseWriter, msg map[string]any) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"total_tokens": 250}})
}

func wireCall(name, args string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) {
		completion(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": "call_" + name, "type": "function",
				"function": map[string]any{"name": name, "arguments": args}}}})
	}
}

func wireContent(content string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) {
		completion(w, map[string]any{"role": "assistant", "content": content})
	}
}

func wireStatus(code int, body string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func clientConfig(tools ...Tool) Config {
	cfg := baseConfig(tools...)
	cfg.Protocol = ProtocolAuto
	return cfg
}

func TestClient_NativeTranscriptEndToEnd(t *testing.T) {
	tool := &countingTool{}
	f := newOpenAIFake(t, wireCall("lookup", `{"n":1}`),
		wireCall("submit_conclusion", finalArgs("agree",
			Claim{Text: "lookup ok.", EvidenceIDs: []string{"E2"}})))
	res, err := Run(context.Background(), f.client(),
		clientConfig(tool.tool("lookup", 1, true, "lookup ok")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopFinal || len(res.Claims) != 1 || f.count() != 2 {
		t.Fatalf("transcript %+v claims %+v requests %d", res.Transcript, res.Claims, f.count())
	}
	second := f.body(1)
	if !strings.Contains(second, `"tool_call_id":"call_lookup"`) ||
		!strings.Contains(second, `"role":"tool"`) {
		t.Fatalf("the tool result was not sent as a tool message: %s", second)
	}
	if res.Transcript.Tokens != 500 {
		t.Fatalf("tokens = %d, want the 2 x 250 reported", res.Transcript.Tokens)
	}
}

func TestClient_UndeclaredToolIsAMalformedReply(t *testing.T) {
	f := newOpenAIFake(t, wireCall("pg_terminate_backend", `{"pid":4242}`),
		wireCall("submit_conclusion", finalArgs("inconclusive")))
	res, err := Run(context.Background(), f.client(), clientConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Rejected[RejectMalformedReply] != 1 || res.Transcript.Stop != StopFinal {
		t.Fatalf("transcript = %+v", res.Transcript)
	}
	if !strings.Contains(f.body(1), "listed tools") {
		t.Fatal("the corrective message does not name the listed tools")
	}
}

func TestClient_MalformedArgumentsAreAMalformedReply(t *testing.T) {
	tool := &countingTool{}
	f := newOpenAIFake(t, wireCall("lookup", `{"n":`),
		wireCall("submit_conclusion", finalArgs("inconclusive")))
	res, err := Run(context.Background(), f.client(),
		clientConfig(tool.tool("lookup", 1, true, "x")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Rejected[RejectMalformedReply] != 1 || tool.count() != 0 {
		t.Fatalf("transcript %+v, tool runs %d", res.Transcript, tool.count())
	}
}

func TestClient_MalformedProviderJSONFallsBackThenStops(t *testing.T) {
	f := newOpenAIFake(t, func(w http.ResponseWriter, _ string) {
		_, _ = w.Write([]byte(`{"choices": [ {"message": `))
	})
	res, err := Run(context.Background(), f.client(), clientConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopProvider || f.count() != 2 ||
		res.Transcript.Protocol != ProtocolJSON {
		t.Fatalf("stop %s protocol %s after %d requests; want one JSON-protocol retry",
			res.Transcript.Stop, res.Transcript.Protocol, f.count())
	}
}

func TestClient_FencedJSONActionsAfterToolsAreRefused(t *testing.T) {
	tool := &countingTool{}
	f := newOpenAIFake(t,
		wireStatus(http.StatusBadRequest, `{"error":{"message":"tools are not supported"}}`),
		wireContent("Sure.\n```json\n{\"tool\":\"lookup\",\"args\":{\"n\":2}}\n```"),
		wireContent("```json\n{\"tool\":\"submit_conclusion\",\"args\":"+
			finalArgs("agree", Claim{Text: "lookup ok.", EvidenceIDs: []string{"E2"}})+"}\n```"))
	res, err := Run(context.Background(), f.client(),
		clientConfig(tool.tool("lookup", 1, true, "lookup ok")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Protocol != ProtocolJSON || len(res.Claims) != 1 || tool.count() != 1 {
		t.Fatalf("transcript %+v claims %+v", res.Transcript, res.Claims)
	}
	if strings.Contains(f.body(1), `"tools"`) {
		t.Fatal("the JSON-protocol request still offered native tools")
	}
}

func TestClient_EmptyReplyIsRejected(t *testing.T) {
	f := newOpenAIFake(t, wireContent(""),
		wireCall("submit_conclusion", finalArgs("inconclusive")))
	res, err := Run(context.Background(), f.client(), clientConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Rejected[RejectEmptyReply] != 1 || res.Transcript.Stop != StopFinal {
		t.Fatalf("transcript = %+v", res.Transcript)
	}
}

func TestClient_RateLimitStopsAtOnce(t *testing.T) {
	f := newOpenAIFake(t, wireStatus(http.StatusTooManyRequests,
		`{"error":{"message":"rate limited"}}`))
	res, err := Run(context.Background(), f.client(), clientConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopRateLimited || f.count() != 1 {
		t.Fatalf("stop %s after %d requests", res.Transcript.Stop, f.count())
	}
}

func TestClient_SlowProviderTimesOutWithinTheStep(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := newOpenAIFake(t, func(w http.ResponseWriter, _ string) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		completion(w, map[string]any{"role": "assistant", "content": "late"})
	})
	cfg := clientConfig()
	cfg.Budget.StepTimeout = 200 * time.Millisecond
	start := time.Now()
	res, err := Run(context.Background(), f.client(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcript.Stop != StopTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("stop %s after %s, want timeout within the step", res.Transcript.Stop,
			time.Since(start))
	}
}

func TestClient_ToolCallLoopNeverConcludingStops(t *testing.T) {
	free := &countingTool{}
	n := 0
	var mu sync.Mutex
	f := newOpenAIFake(t, func(w http.ResponseWriter, body string) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		wireCall("graph", `{"n":`+itoa(k)+`}`)(w, body)
	})
	cfg := clientConfig(free.tool("graph", 0, false, "y"))
	cfg.Budget.MaxSteps = 4
	res, err := Run(context.Background(), f.client(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The last step offers only the final tool, so the client itself
	// refuses the graph call there as undeclared.
	if res.Transcript.Stop != StopMaxSteps || f.count() != 4 || res.Final != nil {
		t.Fatalf("stop %s after %d requests, final %s", res.Transcript.Stop, f.count(),
			res.Final)
	}
	if !strings.Contains(f.body(3), `"tool_choice":"required"`) {
		t.Fatalf("the last step does not require the final tool: %s", f.body(3))
	}
}
