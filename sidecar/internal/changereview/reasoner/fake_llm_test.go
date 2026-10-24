package reasoner

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

// fakeLLM is an OpenAI-compatible chat-completions server (the scripted
// pattern of internal/ask/fake_llm_test.go) answering the n-th request
// with the n-th scripted reply; requests beyond the script get HTTP 500.
// It records every request body so tests assert what the model was
// shown. No test reaches a live provider.

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

func (f *fakeLLM) client() *llm.Client { return f.clientEnabled(true) }

func (f *fakeLLM) clientEnabled(on bool) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: on, Endpoint: f.srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 10_000_000},
		func(string, string, ...any) {})
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeLLM) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

type wireMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

type wireRequest struct {
	Messages []wireMessage `json:"messages"`
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

func writeCompletion(w http.ResponseWriter, msg map[string]any, tokens int) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"total_tokens": tokens},
	})
}

type toolCall struct{ name, args string }

// callTools answers with native tool calls built from the request.
func callTools(build func(body string) []toolCall) fakeReply {
	return func(w http.ResponseWriter, body string) {
		var calls []map[string]any
		for i, c := range build(body) {
			calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d", i+1),
				"type": "function", "function": map[string]any{"name": c.name,
					"arguments": c.args}})
		}
		writeCompletion(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": calls}, 300)
	}
}

// calls is callTools with fixed calls.
func calls(c ...toolCall) fakeReply {
	return callTools(func(string) []toolCall { return c })
}

func jsonArgs(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// submit replies with the submit_review final tool built from the request.
func submit(build func(body string) map[string]any) fakeReply {
	return callTools(func(body string) []toolCall {
		return []toolCall{{name: FinalTool, args: jsonArgs(build(body))}}
	})
}

// emptySubmit concludes with no findings.
func emptySubmit() fakeReply {
	return submit(func(string) map[string]any {
		return map[string]any{"findings": []any{}, "intent": "unclear"}
	})
}

func contentReply(content string) fakeReply {
	return func(w http.ResponseWriter, _ string) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": content}, 300)
	}
}

func statusReply(code int) fakeReply {
	return func(w http.ResponseWriter, _ string) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"provider says no"}}`))
	}
}

func slowReply(d time.Duration, next fakeReply) fakeReply {
	return func(w http.ResponseWriter, body string) {
		time.Sleep(d)
		next(w, body)
	}
}

// aliasFor finds the alias the conversation gave the evidence with this
// id, e.g. "E4" for "E4 [inspect:0a1b2c3d4e5f ...".
func aliasFor(t *testing.T, body, evidenceID string) string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(evidenceID) + `[\] ]`)
	m := re.FindStringSubmatch(prompt(t, body))
	if m == nil {
		t.Errorf("the conversation shows no evidence %s:\n%s", evidenceID, prompt(t, body))
		return "E0"
	}
	return m[1]
}

// aliasOfKind finds the alias of the last evidence whose id starts with
// prefix ("tool:queries_touching.", "whatif:tool.").
func aliasOfKind(t *testing.T, body, prefix string) string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(prefix) + `[0-9a-z.]+[\] ]`)
	all := re.FindAllStringSubmatch(prompt(t, body), -1)
	if len(all) == 0 {
		t.Errorf("the conversation shows no evidence %s*:\n%s", prefix, prompt(t, body))
		return "E0"
	}
	return all[len(all)-1][1]
}

// dataBlocks is the content inside every <data ...> block of a request,
// and what is left outside them.
func dataBlocks(t *testing.T, body string) (inside, outside string) {
	t.Helper()
	re := regexp.MustCompile(`(?s)<data label="[^"]*">(.*?)</data>`)
	var in strings.Builder
	for _, m := range decodeRequest(t, body).Messages {
		for _, blk := range re.FindAllStringSubmatch(m.Content, -1) {
			in.WriteString(blk[1])
		}
	}
	return in.String(), re.ReplaceAllString(prompt(t, body), "")
}
