package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Typed runbook and incident memory routes (AI-SRE-SPEC §7.1, §9):
// viewers list and read runbooks, run history and similar incidents;
// operators create and edit drafts, compile English, retire runbooks and
// record investigation outcomes; only admins sign. Every route is scoped
// to the named database, and errors carry canonical codes.

const rbDef = `{"name":"Idle holder","trigger":{"kinds":["lock_blocking"],` +
	`"nodes":["idle_in_tx_holder"]},"start":"read_chains","nodes":[` +
	`{"id":"read_chains","type":"probe","probe":"lock_chains","next":"is_idle"},` +
	`{"id":"is_idle","type":"decision","when":{"op":"hypothesis",` +
	`"node":"idle_in_tx_holder","in":["root_cause"]},"then":"end_tx","else":"esc"},` +
	`{"id":"end_tx","type":"proposal","proposal":{"kind":"operator_step",` +
	`"node":"idle_in_tx_holder"}},` +
	`{"id":"esc","type":"proposal","proposal":{"kind":"escalate"}}]}`

func rbHash(t *testing.T, def string) string {
	t.Helper()
	d, err := runbook.Decode([]byte(def))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := d.Hash()
	return h
}

func rbCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: response %q: %v", method, path, w.Body.String(), err)
		}
	}
	return w.Code, out
}

const rbBase = "/api/v1/databases/orders/runbooks"

func createRB(t *testing.T, mgr *fleet.DatabaseManager) string {
	t.Helper()
	code, out := rbCall(t, sreRouter(t, mgr, testOperatorUser()), "POST", rbBase,
		`{"definition":`+rbDef+`}`)
	if code != 201 || out["id"] == nil {
		t.Fatalf("create = %d %v", code, out)
	}
	return out["id"].(string)
}

func TestRunbookAPI_RolesPerRoute(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	id := createRB(t, mgr)
	hash := rbHash(t, rbDef)
	sign := fmt.Sprintf(`{"version":1,"content_hash":%q}`, hash)
	viewer := sreRouter(t, mgr, testViewerUser())
	operator := sreRouter(t, mgr, testOperatorUser())
	forbidden := []struct {
		h      http.Handler
		method string
		path   string
		body   string
	}{
		{viewer, "POST", rbBase, `{"definition":` + rbDef + `}`},
		{viewer, "POST", rbBase + "/compile", `{"text":"read locks"}`},
		{viewer, "POST", rbBase + "/" + id + "/versions",
			`{"base_version":1,"definition":` + rbDef + `}`},
		{viewer, "POST", rbBase + "/" + id + "/retire", ``},
		{viewer, "POST", rbBase + "/" + id + "/sign", sign},
		{operator, "POST", rbBase + "/" + id + "/sign", sign},
	}
	for _, c := range forbidden {
		if code, _ := rbCall(t, c.h, c.method, c.path, c.body); code != 403 {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, code)
		}
	}
	for _, path := range []string{rbBase, rbBase + "/" + id, rbBase + "/" + id + "/runs"} {
		if code, out := rbCall(t, viewer, "GET", path, ""); code != 200 {
			t.Errorf("viewer GET %s = %d %v", path, code, out)
		}
	}
	if code, _ := rbCall(t, sreRouter(t, mgr, nil), "GET", rbBase, ""); code != 401 {
		t.Errorf("unauthenticated list = %d, want 401", code)
	}
	code, out := rbCall(t, sreRouter(t, mgr, testAdminUser()), "POST",
		rbBase+"/"+id+"/sign", sign)
	if code != 200 || out["runnable"] != true || out["status"] != "signed" {
		t.Fatalf("admin sign = %d %v", code, out)
	}
	latest := out["latest"].(map[string]any)
	if latest["signed_by"] != "user:1" || latest["content_hash"] != hash {
		t.Fatalf("signed version = %v, want signer user:1 over %s", latest, hash)
	}
}

func TestRunbookAPI_DraftEditAndConflicts(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	id := createRB(t, mgr)
	op := sreRouter(t, mgr, testOperatorUser())
	edited := strings.Replace(rbDef, `"else":"esc"`, `"else":"end_tx"`, 1)
	edited = strings.Replace(edited, `,{"id":"esc","type":"proposal","proposal":`+
		`{"kind":"escalate"}}`, ``, 1)
	code, out := rbCall(t, op, "POST", rbBase+"/"+id+"/versions",
		`{"base_version":1,"definition":`+edited+`}`)
	if code != 200 || out["latest_version"] != float64(2) || out["runnable"] != false {
		t.Fatalf("revise = %d %v", code, out)
	}
	code, out = rbCall(t, op, "POST", rbBase+"/"+id+"/versions",
		`{"base_version":1,"definition":`+edited+`}`)
	if code != 409 || out["code"] != "version_conflict" {
		t.Fatalf("stale revise = %d %v, want 409 version_conflict", code, out)
	}
	admin := sreRouter(t, mgr, testAdminUser())
	code, out = rbCall(t, admin, "POST", rbBase+"/"+id+"/sign",
		fmt.Sprintf(`{"version":2,"content_hash":%q}`, rbHash(t, rbDef)))
	if code != 409 || out["code"] != "hash_mismatch" {
		t.Fatalf("sign over the old hash = %d %v, want 409 hash_mismatch", code, out)
	}
	sign2 := fmt.Sprintf(`{"version":2,"content_hash":%q}`, rbHash(t, edited))
	if code, out = rbCall(t, admin, "POST", rbBase+"/"+id+"/sign", sign2); code != 200 {
		t.Fatalf("sign v2 = %d %v", code, out)
	}
	if code, out = rbCall(t, admin, "POST", rbBase+"/"+id+"/sign", sign2); code != 409 ||
		out["code"] != "already_signed" {
		t.Fatalf("sign twice = %d %v, want 409 already_signed", code, out)
	}
	if code, out = rbCall(t, op, "POST", rbBase+"/"+id+"/retire", ""); code != 200 ||
		out["status"] != "retired" {
		t.Fatalf("retire = %d %v", code, out)
	}
	if code, out = rbCall(t, op, "POST", rbBase+"/"+id+"/retire", ""); code != 409 ||
		out["code"] != "retired" {
		t.Fatalf("retire twice = %d %v, want 409 retired", code, out)
	}
}

func TestRunbookAPI_InvalidRequests(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	op := sreRouter(t, mgr, testOperatorUser())
	bad := strings.Replace(rbDef, `"lock_chains"`, `"pg_sleep"`, 1)
	code, out := rbCall(t, op, "POST", rbBase, `{"definition":`+bad+`}`)
	problems, _ := out["problems"].([]any)
	if code != 400 || out["code"] != "invalid_request" || len(problems) == 0 ||
		problems[0].(map[string]any)["code"] != "unknown_probe" {
		t.Fatalf("invalid definition = %d %v, want 400 with problems", code, out)
	}
	for name, body := range map[string]string{
		"unknown field": `{"definition":` + rbDef + `,"sql":"x"}`,
		"no definition": `{}`,
		"not json":      `{`,
		"field in def": `{"definition":` +
			strings.Replace(rbDef, `"start"`, `"sql":1,"start"`, 1) + `}`,
	} {
		if code, out := rbCall(t, op, "POST", rbBase, body); code != 400 {
			t.Errorf("%s: create = %d %v, want 400", name, code, out)
		}
	}
	missing := rbBase + "/" + string(sre.NewUUID())
	if code, out := rbCall(t, op, "GET", missing, ""); code != 404 ||
		out["code"] != "not_found" {
		t.Errorf("unknown runbook = %d %v, want 404", code, out)
	}
	if code, _ := rbCall(t, op, "GET", rbBase+"/not-a-uuid", ""); code != 400 {
		t.Errorf("malformed id = %d, want 400", code)
	}
	code, _ = rbCall(t, op, "POST", rbBase+"/compile", `{"text":"x","extra":1}`)
	if code != 400 {
		t.Errorf("compile with an unknown field = %d, want 400", code)
	}
}

func TestRunbookAPI_ScopedToTheNamedDatabase(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	id := createRB(t, mgr)
	h := sreRouter(t, mgr, testAdminUser())
	other := "/api/v1/databases/billing/runbooks/" + id
	for _, c := range []struct{ method, path, body string }{
		{"GET", other, ""}, {"GET", other + "/runs", ""}, {"POST", other + "/retire", ""},
		{"POST", other + "/sign", fmt.Sprintf(`{"version":1,"content_hash":%q}`,
			rbHash(t, rbDef))},
	} {
		if code, out := rbCall(t, h, c.method, c.path, c.body); code != 404 {
			t.Errorf("%s %s = %d %v, want 404", c.method, c.path, code, out)
		}
	}
	code, out := rbCall(t, h, "GET", "/api/v1/databases/billing/runbooks", "")
	if items, _ := out["items"].([]any); code != 200 || items == nil || len(items) != 0 {
		t.Errorf("billing lists %v", out)
	}
	if code, _ := rbCall(t, h, "GET", "/api/v1/databases/nope/runbooks", ""); code != 404 {
		t.Errorf("unknown database = %d, want 404", code)
	}
}

func TestRunbookAPI_CompileNeedsAModel(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	code, out := rbCall(t, sreRouter(t, mgr, testOperatorUser()), "POST",
		rbBase+"/compile", `{"text":"Read the lock chains."}`)
	if code != 503 || out["code"] != "missing_capability" {
		t.Fatalf("compile without a model = %d %v, want 503 missing_capability", code, out)
	}
}

func TestRunbookAPI_CompileWithAModel(t *testing.T) {
	replies := []string{`{"name":"x"}`, `{"name":"x"}`, rbDef}
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		args := replies[min(int(n.Add(1))-1, len(replies)-1)]
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"message": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
				"id": "c", "type": "function", "function": map[string]any{
					"name": "submit_runbook", "arguments": args}}}},
			"finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 100}})
	}))
	defer srv.Close()
	mgr := fleet.NewManager(config.DefaultConfig())
	modelInstance(t, mgr, "orders", srv.URL)
	op := sreRouter(t, mgr, testOperatorUser())
	code, out := rbCall(t, op, "POST", rbBase+"/compile", `{"text":"Read the locks."}`)
	if code != 422 || out["code"] != "compile_rejected" ||
		out["reason"] != "invalid_definition" {
		t.Fatalf("invalid compile = %d %v, want 422 compile_rejected", code, out)
	}
	code, out = rbCall(t, op, "POST", rbBase+"/compile", `{"text":"Read the locks."}`)
	latest, _ := out["latest"].(map[string]any)
	if code != 201 || latest["source"] != "compiled" || out["runnable"] != false ||
		latest["source_text"] != "Read the locks." {
		t.Fatalf("compile = %d %v, want 201 compiled draft", code, out)
	}
}

func modelInstance(t *testing.T, mgr *fleet.DatabaseManager, name, url string) {
	t.Helper()
	pool := surfacePool(t)
	l := sre.DefaultLimits()
	l.DatabaseDailyTokens, l.DeploymentDailyTokens = 1_000_000, 1_000_000
	st, err := sre.NewPostgresStore(pool, l)
	if err != nil {
		t.Fatal(err)
	}
	model := llm.New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
		func(string, string, ...any) {})
	cc := sre.DefaultCoordinatorConfig(fmt.Sprintf("api-model:%s:%d", name,
		time.Now().UnixNano()))
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: lockChainRunner{}, Config: cc, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st)})
}

func TestMemoryAPI_SimilarAndOutcome(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	base := "/api/v1/databases/orders/investigations/" + string(orders.ID)
	viewer := sreRouter(t, mgr, testViewerUser())
	code, out := rbCall(t, viewer, "GET", base+"/similar", "")
	if code != 200 || out["items"] == nil || len(out["items"].([]any)) != 0 {
		t.Fatalf("similar = %d %v, want an empty list", code, out)
	}
	if code, _ := rbCall(t, viewer, "POST", base+"/outcome",
		`{"verdict":"confirmed"}`); code != 403 {
		t.Fatalf("viewer outcome = %d, want 403", code)
	}
	op := sreRouter(t, mgr, testOperatorUser())
	code, out = rbCall(t, op, "POST", base+"/outcome", `{"verdict":"confirmed"}`)
	if code != 200 || out["verdict"] != "confirmed" || out["actor"] != "user:2" {
		t.Fatalf("outcome = %d %v", code, out)
	}
	code, out = rbCall(t, op, "POST", base+"/outcome", `{"verdict":"maybe"}`)
	if code != 400 {
		t.Fatalf("bad verdict = %d %v, want 400", code, out)
	}
	other := "/api/v1/databases/billing/investigations/" + string(orders.ID)
	if code, _ := rbCall(t, op, "GET", other+"/similar", ""); code != 404 {
		t.Fatalf("similar under another database = %d, want 404", code)
	}
}
