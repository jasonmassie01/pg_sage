package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// mcp-07: POST /api/v1/databases/{db}/ask gets ask.max_seconds_per_question
// plus 5 s through the real API middleware chain; every other route keeps
// the 30 s request deadline. The handler outlives the server's write
// timeout, so a slow model's answer still reaches the browser.

func askSecondsConfig(seconds int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Ask.MaxSecondsPerQuestion = seconds
	return cfg
}

// asViewer is the session middleware of these tests: a signed-in viewer.
func asViewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, withUser(r, viewerUser()))
	})
}

func routedAPI(cfg *config.Config, mux *http.ServeMux) http.Handler {
	return wrapAPIHandler(mux, []func(http.Handler) http.Handler{asViewer},
		newCrossOriginProtection(cfg, false), askRequestTimeout(cfg), accessLogOn(cfg))
}

func TestAskRoute_DeadlineIsTheSettingPlusFiveSeconds(t *testing.T) {
	var (
		mu   sync.Mutex
		left = map[string]time.Duration{}
	)
	record := func(w http.ResponseWriter, r *http.Request) {
		d, ok := r.Context().Deadline()
		mu.Lock()
		left[r.Method+" "+r.URL.Path] = -1
		if ok {
			left[r.Method+" "+r.URL.Path] = time.Until(d)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", record)
	cases := []struct {
		cfg          *config.Config
		method, path string
		want         time.Duration
	}{
		{askSecondsConfig(40), "POST", "/api/v1/databases/testdb/ask", 45 * time.Second},
		{askSecondsConfig(300), "POST", "/api/v1/databases/prod_1/ask", 305 * time.Second},
		{nil, "POST", "/api/v1/databases/testdb/ask", 65 * time.Second},
		{askSecondsConfig(40), "GET", "/api/v1/databases/testdb/ask", 30 * time.Second},
		{askSecondsConfig(40), "POST", "/api/v1/databases/testdb/ask/conversations",
			30 * time.Second},
		{askSecondsConfig(40), "POST", "/api/v1/databases/a/b/ask", 30 * time.Second},
		{askSecondsConfig(40), "GET", "/api/v1/findings", 30 * time.Second},
		{askSecondsConfig(40), "POST", "/api/v1/findings/1/suppress", 30 * time.Second},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		routedAPI(c.cfg, mux).ServeHTTP(w, req)
		mu.Lock()
		got, seen := left[c.method+" "+c.path]
		mu.Unlock()
		if !seen {
			t.Errorf("%s %s did not reach the handler (%d)", c.method, c.path, w.Code)
			continue
		}
		if got > c.want || got < c.want-2*time.Second {
			t.Errorf("%s %s (setting %v): deadline in %s, want %s", c.method, c.path,
				c.cfg != nil, got, c.want)
		}
	}
}

func TestIsAskQuestion_OnlyThePostOfOneDatabase(t *testing.T) {
	// The mux redirects unclean paths such as /databases//ask before any
	// handler, so the matcher's empty-name case is pinned here.
	for path, want := range map[string]bool{"/api/v1/databases/db1/ask": true,
		"/api/v1/databases//ask": false, "/api/v1/databases/ask": false,
		"/api/v1/databases/a/b/ask": false, "/api/v1/databases/db1/ask/": false,
		"/api/v1/databases/db1/asks": false, "/api/v2/databases/db1/ask": false} {
		if got := isAskQuestion(httptest.NewRequest("POST", path, nil)); got != want {
			t.Errorf("POST %s: %v, want %v", path, got, want)
		}
	}
	if isAskQuestion(httptest.NewRequest("PUT", "/api/v1/databases/db1/ask", nil)) {
		t.Error("PUT counted as a question")
	}
}

func TestAskRoute_EventsStreamKeepsNoDeadline(t *testing.T) {
	var sawDeadline bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
		_, sawDeadline = r.Context().Deadline()
	})
	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	routedAPI(askSecondsConfig(40), mux).ServeHTTP(httptest.NewRecorder(), req)
	if sawDeadline {
		t.Fatal("/api/v1/events got a request deadline")
	}
}

// scriptedModel is an OpenAI-compatible server answering request n after
// delays[n] with replies[n] (a tool call); later requests get HTTP 500.
type scriptedModel struct {
	srv *httptest.Server
	mu  sync.Mutex
	n   int
}

type scriptedReply struct {
	delay      time.Duration
	tool, args string
}

func newScriptedModel(t *testing.T, script ...scriptedReply) *scriptedModel {
	t.Helper()
	m := &scriptedModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		m.mu.Lock()
		n := m.n
		m.n++
		m.mu.Unlock()
		if n >= len(script) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		select {
		case <-time.After(script[n].delay):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{
				"role": "assistant", "content": "", "tool_calls": []map[string]any{{
					"id": "c1", "type": "function", "function": map[string]any{
						"name": script[n].tool, "arguments": script[n].args}}}}}},
			"usage": map[string]int{"total_tokens": 250}})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// routedAskAPI is the real ask routes behind the real middleware chain,
// over a live sage schema.
func routedAskAPI(t *testing.T, seconds int, model *scriptedModel) http.Handler {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	cfg := askSecondsConfig(seconds)
	svc, err := ask.New(ask.Deps{Database: "testdb", Pool: pool, Config: cfg.Ask,
		Settings: cfg, Model: llm.New(&config.LLMConfig{Enabled: true,
			Endpoint: model.srv.URL, APIKey: "k", Model: "m", TimeoutSeconds: 20,
			TokenBudgetDaily: 1_000_000}, func(string, string, ...any) {})})
	if err != nil {
		t.Fatal(err)
	}
	reg := ask.NewRegistry()
	reg.Set("testdb", svc)
	mux := http.NewServeMux()
	registerAskRoutes(mux, reg)
	return routedAPI(cfg, mux)
}

const groundedAnswer = `{"claims":[{"text":"No open findings were reported.",` +
	`"evidence_ids":["E1"]}],"not_observed":[]}`

func TestAskRoute_SlowModelBoundary(t *testing.T) {
	// The model concludes 35 s in, over three steps under the 15 s step
	// bound: inside a 40 s question, outside a 30 s one.
	script := []scriptedReply{{12 * time.Second, "list_findings", `{}`},
		{12 * time.Second, "list_facts", `{}`}, {11 * time.Second, "answer", groundedAnswer}}
	for _, c := range []struct {
		seconds      int
		status, stop string
	}{{40, ask.StatusAnswered, "final"}, {30, ask.StatusIncomplete, "wall_clock"}} {
		t.Run(c.status, func(t *testing.T) {
			t.Parallel()
			h := routedAskAPI(t, c.seconds, newScriptedModel(t, script...))
			req := httptest.NewRequest("POST", "/api/v1/databases/testdb/ask",
				bytes.NewReader([]byte(`{"question":"Is anything open?"}`)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(w, req)
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusOK || body["status"] != c.status ||
				body["stop"] != c.stop || body["id"] == nil {
				t.Fatalf("setting %d s: %d %v", c.seconds, w.Code, body)
			}
			if took := time.Since(start); took > time.Duration(c.seconds+5)*time.Second {
				t.Fatalf("setting %d s: answered after %s", c.seconds, took)
			}
		})
	}
}

func TestAskRoute_AnswerOutlivesTheServerTimeouts(t *testing.T) {
	// A 3.5 s answer on a server whose read and write timeouts are 2 s
	// (the production server's are 30 s and 60 s; a question may take 300).
	h := routedAskAPI(t, 15, newScriptedModel(t, scriptedReply{3500 * time.Millisecond,
		"answer", `{"claims":[],"not_observed":["Nothing was observed."]}`}))
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 2*time.Second, 2*time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/v1/databases/testdb/ask",
		bytes.NewReader([]byte(`{"question":"Anything?"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("the answer did not reach the client: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body["status"] != ask.StatusNotObserved {
		t.Fatalf("answer: %d %v", resp.StatusCode, body)
	}
}
