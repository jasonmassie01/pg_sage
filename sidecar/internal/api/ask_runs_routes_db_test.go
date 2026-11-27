package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// mcp-07b: asynchronous Ask Sage runs over REST. POST .../ask/runs is
// 202 with {run_id, conversation_id}; GET .../ask/runs/{id} is the state,
// the progress and, once done, the answer; POST .../ask/runs/{id}/cancel
// cancels. Another actor's run is 404. The routes keep the 30 s request
// deadline; the run lives on the service's own context.

type runsAPI struct {
	pool          *pgxpool.Pool
	viewer, other http.Handler
}

func newRunsAPI(t *testing.T, model *scriptedModel, maxRuns int) runsAPI {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	if _, err := pool.Exec(ctx, `TRUNCATE sage.ask_conversations CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	cfg := askSecondsConfig(15)
	cfg.Ask.MaxConcurrentRuns = maxRuns
	svc, err := ask.New(ask.Deps{Database: "testdb", Pool: pool, Config: cfg.Ask,
		Settings: cfg, Instance: "api-test", Model: llm.New(&config.LLMConfig{
			Enabled: true, Endpoint: model.srv.URL, APIKey: "k", Model: "m",
			TimeoutSeconds: 20, TokenBudgetDaily: 1_000_000}, func(string, string, ...any) {})})
	if err != nil {
		t.Fatal(err)
	}
	reg := ask.NewRegistry()
	reg.Set("testdb", svc)
	mux := http.NewServeMux()
	registerAskRoutes(mux, reg)
	wrap := func(u *auth.User) http.Handler {
		return wrapAPIHandler(mux, []func(http.Handler) http.Handler{
			func(next http.Handler) http.Handler { return asUser(next, u) }},
			newCrossOriginProtection(cfg, false), askRequestTimeout(cfg), accessLogOn(cfg))
	}
	return runsAPI{pool: pool, viewer: wrap(viewerUser()), other: wrap(operatorUser())}
}

func call(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any,
	http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if method == "POST" { // the API refuses a POST without a JSON content type
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Header()
}

const runsPath = "/api/v1/databases/testdb/ask/runs"

func startRun(t *testing.T, h http.Handler, body string) (string, string) {
	t.Helper()
	code, out, _ := call(t, h, "POST", runsPath, body)
	runID, _ := out["run_id"].(string)
	conv, _ := out["conversation_id"].(string)
	if code != http.StatusAccepted || runID == "" || conv == "" {
		t.Fatalf("POST runs = %d %v", code, out)
	}
	return runID, conv
}

func pollUntilEnded(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, out, _ := call(t, h, "GET", runsPath+"/"+id, "")
		if code != http.StatusOK {
			t.Fatalf("GET run = %d %v", code, out)
		}
		switch out["state"] {
		case "done", "cancelled", "failed", "interrupted":
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still %v", id, out["state"])
		}
		time.Sleep(25 * time.Millisecond)
	}
}

var notObservedReply = scriptedReply{0, "answer",
	`{"claims":[],"not_observed":["Nothing was observed."]}`}

func TestAskRunsRoutes_StartPollDone(t *testing.T) {
	api := newRunsAPI(t, newScriptedModel(t, scriptedReply{300 * time.Millisecond,
		"list_findings", `{}`}, notObservedReply), 4)
	id, conv := startRun(t, api.viewer, `{"question":"Anything open?"}`)
	out := pollUntilEnded(t, api.viewer, id)
	answer, _ := out["answer"].(map[string]any)
	progress, _ := out["progress"].([]any)
	if out["state"] != "done" || out["conversation_id"] != conv || answer == nil ||
		answer["status"] != ask.StatusNotObserved || out["message_id"] == nil ||
		len(progress) != 1 {
		t.Fatalf("done run = %v", out)
	}
	step, _ := progress[0].(map[string]any)
	if step["tool"] != "list_findings" || step["at"] == nil {
		t.Fatalf("progress step = %v", step)
	}
	if code, body, _ := call(t, api.viewer, "POST", runsPath+"/"+id+"/cancel", ""); code !=
		http.StatusConflict {
		t.Fatalf("cancel of a done run = %d %v, want 409", code, body)
	}
}

func TestAskRunsRoutes_NotFoundAndInvalid(t *testing.T) {
	api := newRunsAPI(t, newScriptedModel(t, notObservedReply), 4)
	id, _ := startRun(t, api.viewer, `{"question":"Mine?"}`)
	pollUntilEnded(t, api.viewer, id)
	for _, c := range []struct {
		h            http.Handler
		method, path string
	}{
		{api.other, "GET", runsPath + "/" + id},
		{api.other, "POST", runsPath + "/" + id + "/cancel"},
		{api.viewer, "GET", runsPath + "/6f1c1a52-6d1f-4d55-9a43-6a0f3c1d2e3f"},
		{api.viewer, "GET", runsPath + "/not-a-uuid"},
		{api.viewer, "GET", "/api/v1/databases/nope/ask/runs/" + id},
	} {
		if code, body, _ := call(t, c.h, c.method, c.path, ""); code != http.StatusNotFound {
			t.Errorf("%s %s = %d %v, want 404", c.method, c.path, code, body)
		}
	}
	long := `{"question":"` + strings.Repeat("q", 8001) + `"}`
	for _, body := range []string{long, `{"question":""}`, `{"q":"x"}`, `not json`} {
		if code, out, _ := call(t, api.viewer, "POST", runsPath, body); code !=
			http.StatusBadRequest {
			t.Errorf("POST runs %.40q = %d %v, want 400", body, code, out)
		}
	}
}

func TestAskRunsRoutes_CapsAre429And409(t *testing.T) {
	slow := scriptedReply{5 * time.Second, "answer",
		`{"claims":[],"not_observed":["Nothing was observed."]}`}
	api := newRunsAPI(t, newScriptedModel(t, slow, slow), 1)
	id, conv := startRun(t, api.viewer, `{"question":"Slow?"}`)
	code, out, hdr := call(t, api.viewer, "POST", runsPath, `{"question":"Another?"}`)
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" ||
		out["code"] != "ask_busy" {
		t.Fatalf("a run over the cap = %d %v (Retry-After %q)", code, out,
			hdr.Get("Retry-After"))
	}
	code, out, _ = call(t, api.viewer, "POST", runsPath,
		`{"question":"Again?","conversation_id":"`+conv+`"}`)
	if code != http.StatusConflict && code != http.StatusTooManyRequests {
		t.Fatalf("a second run on the conversation = %d %v", code, out)
	}
	code, out, _ = call(t, api.viewer, "POST", runsPath+"/"+id+"/cancel", "")
	if code != http.StatusOK || out["state"] != "cancelled" {
		t.Fatalf("cancel = %d %v", code, out)
	}
}

func TestAskRunsRoutes_SecondRunOnAConversationIs409(t *testing.T) {
	slow := scriptedReply{5 * time.Second, "answer",
		`{"claims":[],"not_observed":["Nothing was observed."]}`}
	api := newRunsAPI(t, newScriptedModel(t, slow, slow), 4)
	id, conv := startRun(t, api.viewer, `{"question":"Slow?"}`)
	code, out, _ := call(t, api.viewer, "POST", runsPath,
		`{"question":"Again?","conversation_id":"`+conv+`"}`)
	if code != http.StatusConflict || out["code"] != "ask_conflict" {
		t.Fatalf("a second run on the conversation = %d %v, want 409", code, out)
	}
	if code, _, _ := call(t, api.viewer, "POST", runsPath+"/"+id+"/cancel", ""); code !=
		http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
}

func TestAskRunsRoutes_FailedRunPollsAsAState(t *testing.T) {
	api := newRunsAPI(t, newScriptedModel(t), 4)
	ctx := t.Context()
	var conv, id string
	if err := api.pool.QueryRow(ctx, `INSERT INTO sage.ask_conversations (actor, title)
		VALUES ('user:3', 't') RETURNING id::text`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if err := api.pool.QueryRow(ctx, `INSERT INTO sage.ask_runs (id, conversation_id, actor,
		question, read_only, state, error, instance, finished_at)
		VALUES (gen_random_uuid(), $1::uuid, 'user:3', 'q', false, 'failed',
		'ask: unavailable: record progress: down', 'api-test', now())
		RETURNING id::text`, conv).Scan(&id); err != nil {
		t.Fatal(err)
	}
	code, out, _ := call(t, api.viewer, "GET", runsPath+"/"+id, "")
	if code != http.StatusOK || out["state"] != "failed" ||
		!strings.Contains(fmt.Sprint(out["error"]), "record progress") {
		t.Fatalf("poll of a failed run = %d %v, want 200 with the state", code, out)
	}
}
