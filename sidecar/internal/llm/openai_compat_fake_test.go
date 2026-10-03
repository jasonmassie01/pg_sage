package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// fakeOpenAI is an OpenAI-compatible chat-completions server that, like
// api.openai.com for current models, rejects request shapes the model does
// not support. It never reaches a live provider.
type fakeOpenAI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	raws  []string
	calls atomic.Int32
	// maxTokensErr, when set, is the 400 body for any request carrying
	// max_tokens.
	maxTokensErr string
	// toolEffortErr, when set, is the 400 body for a request with tools
	// whose reasoning_effort is not "none".
	toolEffortErr string
	// status is the HTTP status used for the two rejections (400 default).
	status int
	// next, when set, replies to every request the rejections let through.
	next func(w http.ResponseWriter, body map[string]any)
	// reasoning is the reasoning_tokens reported when reasoning_effort is
	// not "none".
	reasoning int
	// gate, when set, is called before a rejection is written.
	gate func()
}

// OpenAI's error bodies for gpt-6-luna, as observed against the live API.
const (
	openAIMaxTokensErr = `{"error":{"message":"Unsupported parameter: 'max_tokens' ` +
		`is not supported with this model. Use 'max_completion_tokens' instead.",` +
		`"type":"invalid_request_error","param":"max_tokens",` +
		`"code":"unsupported_parameter"}}`
	openAIToolEffortErr = `{"error":{"message":"Function tools with reasoning_effort ` +
		`are not supported for gpt-6-luna in /v1/chat/completions. To use ` +
		`function tools, use /v1/responses or set reasoning_effort to 'none'.",` +
		`"type":"invalid_request_error","param":"reasoning_effort","code":null}}`
)

func newFakeOpenAI(t *testing.T, setup func(f *fakeOpenAI)) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{status: http.StatusBadRequest, reasoning: 64}
	if setup != nil {
		setup(f)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	// Learned request shapes are process-wide and keyed by endpoint and
	// model; a later test's server can reuse a closed server's port. A
	// unique base path per fake keeps one test's learning out of the next
	// (the handler serves every path).
	f.srv.URL += fmt.Sprintf("/fake%d", fakeServerSeq.Add(1))
	t.Cleanup(f.srv.Close)
	return f
}

var fakeServerSeq atomic.Int64

func (f *fakeOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.raws = append(f.raws, string(raw))
	f.mu.Unlock()
	if _, ok := body["max_tokens"]; ok && f.maxTokensErr != "" {
		f.reject(w, f.maxTokensErr)
		return
	}
	_, hasTools := body["tools"]
	if hasTools && f.toolEffortErr != "" && body["reasoning_effort"] != "none" {
		f.reject(w, f.toolEffortErr)
		return
	}
	if f.next != nil {
		f.next(w, body)
		return
	}
	f.succeed(w, body)
}

func (f *fakeOpenAI) reject(w http.ResponseWriter, msg string) {
	if f.gate != nil {
		f.gate()
	}
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(msg))
}

// succeed replies with a tool call when tools were offered, else "ok",
// and an OpenAI usage breakdown: 100 prompt + 20 completion tokens, plus
// reasoning tokens unless reasoning_effort is "none".
func (f *fakeOpenAI) succeed(w http.ResponseWriter, body map[string]any) {
	reasoning := f.reasoning
	if body["reasoning_effort"] == "none" {
		reasoning = 0
	}
	msg := map[string]any{"role": "assistant", "content": "ok"}
	reason := "stop"
	if _, ok := body["tools"]; ok {
		msg["content"] = ""
		msg["tool_calls"] = []map[string]any{fnCall("call_1", "get_evidence",
			`{"id":"E1"}`)}
		reason = "tool_calls"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": reason}},
		"usage": map[string]any{"prompt_tokens": 100,
			"completion_tokens": 20 + reasoning, "total_tokens": 120 + reasoning,
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": reasoning}},
	})
}

// request returns the i-th request body as a decoded map.
func (f *fakeOpenAI) request(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.raws) {
		t.Fatalf("request %d not sent (%d requests)", i, len(f.raws))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.raws[i]), &body); err != nil {
		t.Fatalf("request %d is not JSON: %v", i, err)
	}
	return body
}

// keys returns the sorted top-level keys of the i-th request.
func (f *fakeOpenAI) keys(t *testing.T, i int) string {
	t.Helper()
	body := f.request(t, i)
	out := make([]string, 0, len(body))
	for k := range body {
		out = append(out, k)
	}
	sortStrings(out)
	return strings.Join(out, ",")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// capLog records log lines so tests can count adaptation notices.
type capLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *capLog) log(_, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// count returns how many lines contain every one of the substrings.
func (l *capLog) count(subs ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		all := true
		for _, s := range subs {
			all = all && strings.Contains(line, s)
		}
		if all {
			n++
		}
	}
	return n
}

// wireClient builds a client for the fake with the given wire overrides.
func wireClient(url, model, tokenParam, effort string, logFn func(string,
	string, ...any)) *Client {
	return New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "k",
		Model: model, TimeoutSeconds: 5, TokenBudgetDaily: 10_000_000,
		TokenParameter: tokenParam, ToolReasoningEffort: effort}, logFn)
}

func numField(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	v, ok := body[key].(float64)
	if !ok {
		t.Fatalf("request has no numeric %s: %v", key, body)
	}
	return int(v)
}

func failureCount(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}
