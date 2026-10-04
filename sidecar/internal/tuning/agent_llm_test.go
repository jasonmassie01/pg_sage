package tuning

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The agent through the real LLM client against a fake OpenAI-compatible
// server: tool calls, fenced JSON, malformed and empty replies, 429,
// timeouts, proposals against binding facts and unsupported forms.

type fakeOpenAI struct {
	srv    *httptest.Server
	calls  atomic.Int32
	mu     sync.Mutex
	bodies []map[string]any
	reply  func(n int, w http.ResponseWriter, r *http.Request)
}

func newFakeOpenAI(t *testing.T, reply func(int, http.ResponseWriter, *http.Request)) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(f.calls.Add(1))
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		f.reply(n, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenAI) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func completion(w http.ResponseWriter, content string, calls []map[string]any) {
	msg := map[string]any{"role": "assistant", "content": content}
	if calls != nil {
		msg["tool_calls"] = calls
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"total_tokens": 300, "prompt_tokens": 250}})
}

func wireToolCall(name, args string) []map[string]any {
	return []map[string]any{{"id": "call_1", "type": "function",
		"function": map[string]any{"name": name, "arguments": args}}}
}

func llmAgent(t *testing.T, url string, timeout int, h *harness) *Agent {
	t.Helper()
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "k",
		Model: "m", TimeoutSeconds: timeout, TokenBudgetDaily: 10_000_000}, noLog)
	return New(defaultSettings(), Deps{Model: client, Indexes: h.indexes, Facts: h.facts,
		Hints: h.hints, Store: h.store, Now: func() time.Time { return t0 }}, h.logs.fn)
}

const fencedIndexAnswer = "Here is my proposal.\n```json\n{\"proposals\":[{\"type\":" +
	"\"index_create\",\"ddl\":\"CREATE INDEX CONCURRENTLY orders_customer_idx ON " +
	"public.orders (customer_id)\",\"rationale\":\"seq scan\",\"evidence\":[\"S1\",\"R1\"]," +
	"\"expected_change_pct\":-45}]}\n```"

func TestLLM_ToolCallThenFencedAnswer(t *testing.T) {
	h := newHarness(t)
	srv := newFakeOpenAI(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			completion(w, "", wireToolCall("statement", `{"queryid":101}`))
			return
		}
		completion(w, fencedIndexAnswer, nil)
	})
	h.agent = llmAgent(t, srv.srv.URL, 10, h)
	out := tune(t, h)
	if _, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory); !ok {
		t.Fatalf("findings = %+v", out.Findings)
	}
	first := srv.body(0)
	tools, _ := first["tools"].([]any)
	if len(tools) < 6 {
		t.Fatalf("the request offers the read-only tools: %v", first["tools"])
	}
	second := srv.body(1)
	msgs, _ := second["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].(string)
	if last["role"] != "tool" || !strings.Contains(content, "R1") ||
		!strings.Contains(content, "600") {
		t.Fatalf("the tool result goes back to the model: %v", last)
	}
}

func TestLLM_MalformedEmpty429AndTimeoutDegrade(t *testing.T) {
	for name, reply := range map[string]func(int, http.ResponseWriter, *http.Request){
		"malformed": func(_ int, w http.ResponseWriter, _ *http.Request) {
			completion(w, `{"proposals":[{"type":"index_create",`, nil)
		},
		"empty": func(_ int, w http.ResponseWriter, _ *http.Request) {
			completion(w, "", nil)
		},
		"429": func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		},
		"timeout": func(_ int, w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
			completion(w, fencedIndexAnswer, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			srv := newFakeOpenAI(t, reply)
			h.agent = llmAgent(t, srv.srv.URL, 1, h)
			prev, cur := ordersPair()
			start := time.Now()
			out, err := h.agent.Tune(context.Background(), cur, prev)
			if err != nil {
				t.Fatalf("a model failure degrades, it does not fail the cycle: %v", err)
			}
			if len(out.Findings) != 0 {
				t.Fatalf("findings = %+v", out.Findings)
			}
			if time.Since(start) > 2500*time.Millisecond {
				t.Fatalf("the cycle took %s: the per-call timeout must bound it",
					time.Since(start))
			}
			if !h.logs.contains("top_statement:101") {
				t.Fatalf("the failure is logged with its case: %v", h.logs.lines)
			}
		})
	}
}

func TestLLM_ProposalAgainstABindingFact(t *testing.T) {
	h := newHarness(t)
	h.facts.list = []facts.Fact{confirmedFact(12, facts.TypeAppMigrations, facts.KindIndex,
		"public.orders_customer_idx", nil)}
	srv := newFakeOpenAI(t, func(int, http.ResponseWriter, *http.Request) {})
	srv.reply = func(_ int, w http.ResponseWriter, _ *http.Request) {
		completion(w, `{"proposals":[{"type":"index_drop","index":"public.orders_pkey",`+
			`"evidence":["T1"]},{"type":"index_create","ddl":"CREATE INDEX CONCURRENTLY `+
			`orders_customer_idx ON public.orders (customer_id)","evidence":["S1"],`+
			`"expected_change_pct":-40}]}`, nil)
	}
	h.agent = llmAgent(t, srv.srv.URL, 10, h)
	out := tune(t, h)
	if len(out.Findings) != 1 {
		t.Fatalf("findings = %+v: the pkey drop is refused, the index redirected", out.Findings)
	}
	f := out.Findings[0]
	if f.RecommendedSQL != "" || f.Detail["source_fix"] == nil {
		t.Fatalf("finding = %+v", f)
	}
	if _, approval := f.Detail[analyzer.DetailApprovalRequired]; approval {
		t.Fatal("a redirected finding is not an approval request: nothing can run")
	}
}

func TestLLM_UnsupportedFormNeverReachesTheDatabase(t *testing.T) {
	h := newHarness(t)
	srv := newFakeOpenAI(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		completion(w, `{"proposals":[{"type":"index_create","ddl":"CREATE INDEX x ON `+
			`public.orders (customer_id); DROP TABLE public.orders","evidence":["S1"],`+
			`"expected_change_pct":-40},{"type":"sql","sql":"TRUNCATE public.orders",`+
			`"evidence":["S1"],"expected_change_pct":-40}]}`, nil)
	})
	h.agent = llmAgent(t, srv.srv.URL, 10, h)
	out := tune(t, h)
	if len(out.Findings) != 0 || len(h.indexes.admittedDDL()) != 0 {
		t.Fatalf("findings %+v admitted %v: multi-statement and raw SQL are refused "+
			"before admission", out.Findings, h.indexes.admittedDDL())
	}
}
