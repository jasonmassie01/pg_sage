package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Ask Sage over REST (owner decision 5): POST a question per database
// with the session's identity; read one's own conversations and budget.
// Every signed-in role may ask; only operators and admins may propose
// (the propose tools are not offered to viewers). No live LLM: the model
// is a fake OpenAI-compatible server.

type askFakeLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newAskFakeLLM(t *testing.T) *askFakeLLM {
	t.Helper()
	f := &askFakeLLM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		args := `{"claims":[],"not_observed":["Nothing about that was observed."]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{
				"role": "assistant", "content": "", "tool_calls": []map[string]any{{
					"id": "c1", "type": "function", "function": map[string]any{
						"name": "answer", "arguments": args}}}}}},
			"usage": map[string]int{"total_tokens": 250}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *askFakeLLM) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

type askProposerStub struct{}

func (askProposerStub) ProposeFinding(context.Context, int64, string) (ask.Proposal, error) {
	return ask.Proposal{}, ask.ErrRefused
}

type askAPI struct {
	mux   *http.ServeMux
	model *askFakeLLM
}

func newAskAPI(t *testing.T, enabled bool) *askAPI {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	if _, err := pool.Exec(ctx, `TRUNCATE sage.ask_conversations, sage.ask_budget_day
		CASCADE`); err != nil {
		t.Fatal(err)
	}
	m := newAskFakeLLM(t)
	cfg := config.DefaultConfig().Ask
	cfg.Enabled = enabled
	svc, err := ask.New(ask.Deps{Database: "testdb", Pool: pool, Config: cfg,
		Settings: config.DefaultConfig(), Proposer: askProposerStub{},
		Model: llm.New(&config.LLMConfig{Enabled: true, Endpoint: m.srv.URL, APIKey: "k",
			Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
			func(string, string, ...any) {})})
	if err != nil {
		t.Fatal(err)
	}
	reg := ask.NewRegistry()
	reg.Set("testdb", svc)
	a := &askAPI{mux: http.NewServeMux(), model: m}
	registerAskRoutes(a.mux, reg)
	return a
}

func (a *askAPI) do(t *testing.T, user *auth.User, method, path, body string) (int,
	map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

const askPath = "/api/v1/databases/testdb/ask"

func TestAskRoutes_AnyRoleAsksAndGetsAStoredAnswer(t *testing.T) {
	a := newAskAPI(t, true)
	if code, _ := a.do(t, nil, "POST", askPath, `{"question":"Why?"}`); code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	code, body := a.do(t, viewerUser(), "POST", askPath, `{"question":"Why is it slow?"}`)
	if code != http.StatusOK || body["status"] != ask.StatusNotObserved ||
		body["conversation_id"] == "" || body["database"] != "testdb" {
		t.Fatalf("viewer ask: %d %v", code, body)
	}
	if strings.Contains(a.model.lastBody(), "propose_action") {
		t.Fatal("a viewer was offered the propose tool")
	}
	conv := body["conversation_id"].(string)
	code, list := a.do(t, viewerUser(), "GET", askPath+"/conversations", "")
	convs, _ := list["conversations"].([]any)
	if code != http.StatusOK || len(convs) != 1 {
		t.Fatalf("conversations: %d %v", code, list)
	}
	code, thread := a.do(t, viewerUser(), "GET", askPath+"/conversations/"+conv, "")
	answers, _ := thread["answers"].([]any)
	if code != http.StatusOK || len(answers) != 1 {
		t.Fatalf("thread: %d %v", code, thread)
	}
	if code, _ := a.do(t, operatorUser(), "GET", askPath+"/conversations/"+conv,
		""); code != http.StatusNotFound {
		t.Fatalf("another user's thread: %d, want 404", code)
	}
	code, budget := a.do(t, viewerUser(), "GET", askPath+"/budget", "")
	if code != http.StatusOK || budget["user_used"] != float64(250) ||
		budget["user_limit"] != float64(config.DefaultAskDailyTokensPerUser) {
		t.Fatalf("budget: %d %v", code, budget)
	}
}

func TestAskRoutes_OperatorsAreOfferedTheProposeTool(t *testing.T) {
	a := newAskAPI(t, true)
	if code, body := a.do(t, operatorUser(), "POST", askPath,
		`{"question":"Propose the fix"}`); code != http.StatusOK {
		t.Fatalf("operator ask: %d %v", code, body)
	}
	if !strings.Contains(a.model.lastBody(), "propose_action") {
		t.Fatal("an operator was not offered the propose tool")
	}
}

func TestAskRoutes_InvalidRequests(t *testing.T) {
	a := newAskAPI(t, true)
	cases := map[string]struct {
		path, body string
		code       int
		errCode    string
	}{
		"empty question": {askPath, `{"question":"  "}`, 400, "invalid_request"},
		"not json":       {askPath, `question=x`, 400, "invalid_request"},
		"unknown field":  {askPath, `{"question":"x","approve":true}`, 400, "invalid_request"},
		"too large": {askPath, `{"question":"` + strings.Repeat("x", 20_000) + `"}`, 400,
			"invalid_request"},
		"unknown db": {"/api/v1/databases/nope/ask", `{"question":"x"}`, 404,
			"not_found"},
		"bad db name": {"/api/v1/databases/all/ask", `{"question":"x"}`, 400, "invalid_request"},
		"unknown conv": {askPath, `{"question":"x","conversation_id":` +
			`"00000000-0000-0000-0000-000000000000"}`, 404, "not_found"},
	}
	for name, tc := range cases {
		code, body := a.do(t, viewerUser(), "POST", tc.path, tc.body)
		if code != tc.code || body["code"] != tc.errCode {
			t.Errorf("%s: %d %v, want %d %s", name, code, body, tc.code, tc.errCode)
		}
	}
}

func TestAskRoutes_DisabledIsServiceUnavailable(t *testing.T) {
	a := newAskAPI(t, false)
	code, body := a.do(t, viewerUser(), "POST", askPath, `{"question":"x"}`)
	if code != http.StatusServiceUnavailable || body["code"] != "ask_disabled" {
		t.Fatalf("disabled: %d %v", code, body)
	}
}
