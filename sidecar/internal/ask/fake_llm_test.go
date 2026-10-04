package ask

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// fakeLLM is an OpenAI-compatible chat-completions server answering the
// n-th request with the n-th scripted reply (requests beyond the script
// get HTTP 500). It records every request body so tests assert what the
// model was shown. No test reaches a live provider.

type fakeReply func(w http.ResponseWriter, body string)

type fakeLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	script []fakeReply
	bodies []string
}

func newFakeLLM(t *testing.T, script ...fakeReply) *fakeLLM {
	t.Helper()
	f := &fakeLLM{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.bodies)
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		if n >= len(f.script) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.script[n](w, string(raw))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) client() *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: f.srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 10_000_000},
		func(string, string, ...any) {})
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeLLM) body(t *testing.T, i int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("request %d not made (%d requests)", i, len(f.bodies))
	}
	return f.bodies[i]
}

// wireMessage is one message of a recorded request.
type wireMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

type wireRequest struct {
	Messages []wireMessage   `json:"messages"`
	Tools    []wireTool      `json:"tools"`
	Choice   json.RawMessage `json:"tool_choice"`
}

type wireTool struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

func decodeRequest(t *testing.T, body string) wireRequest {
	t.Helper()
	var req wireRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v (%s)", err, body)
	}
	return req
}

// prompt is every message's content of a request, joined.
func prompt(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	for _, m := range decodeRequest(t, body).Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// offeredTools lists the native tool names of a request.
func offeredTools(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, tool := range decodeRequest(t, body).Tools {
		out = append(out, tool.Function.Name)
	}
	return out
}

func writeCompletion(w http.ResponseWriter, msg map[string]any) {
	writeCompletionUsage(w, msg, 300)
}

// writeCompletionUsage reports the given total token usage.
func writeCompletionUsage(w http.ResponseWriter, msg map[string]any, tokens int) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"total_tokens": tokens},
	})
}

type toolCall struct{ name, args string }

// callTools answers with native tool calls, built from the request.
func callTools(build func(body string) []toolCall) fakeReply {
	return func(w http.ResponseWriter, body string) {
		var calls []map[string]any
		for i, c := range build(body) {
			calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d", i+1),
				"type": "function", "function": map[string]any{"name": c.name,
					"arguments": c.args}})
		}
		writeCompletion(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": calls})
	}
}

// calls is callTools with fixed calls.
func calls(c ...toolCall) fakeReply {
	return callTools(func(string) []toolCall { return c })
}

type claimArg struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type answerArgs struct {
	Claims      []claimArg `json:"claims"`
	NotObserved []string   `json:"not_observed,omitempty"`
}

func (a answerArgs) json() string {
	raw, _ := json.Marshal(a)
	return string(raw)
}

// answer replies with the final answer tool call built from the request.
func answer(build func(body string) answerArgs) fakeReply {
	return callTools(func(body string) []toolCall {
		return []toolCall{{name: "answer", args: build(body).json()}}
	})
}

func contentReply(build func(body string) string) fakeReply {
	return func(w http.ResponseWriter, body string) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": build(body)})
	}
}

func statusReply(code int) fakeReply {
	return func(w http.ResponseWriter, _ string) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"tools are not supported"}}`))
	}
}

func slowReply(d time.Duration, next fakeReply) fakeReply {
	return func(w http.ResponseWriter, body string) {
		time.Sleep(d)
		next(w, body)
	}
}

// aliasFor finds the alias the prompt gave the evidence with this id,
// e.g. "E2" for "E2 [finding:42 ...]".
func aliasFor(t *testing.T, body, evidenceID string) string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(evidenceID) + `[\] ]`)
	m := re.FindStringSubmatch(prompt(t, body))
	if m == nil {
		t.Errorf("the prompt shows no evidence %s:\n%s", evidenceID, prompt(t, body))
		return "E0"
	}
	return m[1]
}
