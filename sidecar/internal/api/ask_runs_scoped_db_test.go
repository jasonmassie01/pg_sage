package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-08c x mcp-07b: the asynchronous Ask Sage run routes follow the
// question route's grant rule. With grants on, starting, polling or
// cancelling a run on a database outside the caller's visible set is
// answered like an unknown database (404, the same body) and stores
// nothing; the model is never called. No live LLM: a fake server.
// No concurrent access test here: the grant check is per request and holds
// no state; run concurrency is covered by ask_runs_routes_db_test.go.

const hiddenRunID = "6f1d2b9a-3c4e-4f5a-8b7c-1d2e3f4a5b6c"

func (a *askScopedAPI) do(user *auth.User, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, withUser(req, user))
	return w.Code, w.Body.String()
}

func (a *askScopedAPI) runs(t *testing.T) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.ask_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitRunEnded polls a started run until it leaves queued and running, so
// its goroutine never outlives the test's pool.
func (a *askScopedAPI) waitRunEnded(t *testing.T, user *auth.User, db, id string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, body := a.do(user, http.MethodGet, "/api/v1/databases/"+db+"/ask/runs/"+id, "")
		var out struct {
			State string `json:"state"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("poll run %s on %s: %d %s", id, db, code, body)
		}
		if out.State != "queued" && out.State != "running" {
			return out.State
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s on %s did not end within 30s", id, db)
	return ""
}

func TestAskRunsScoped_HiddenDatabaseIsNotFoundOnEveryRoute(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	a := newAskScopedAPI(t, src)
	scoped := &auth.User{ID: 7, Role: auth.RoleViewer}
	question := `{"question":"Why is it slow?"}`
	routes := []struct{ method, suffix, body string }{
		{http.MethodPost, "/ask/runs", question},
		{http.MethodGet, "/ask/runs/" + hiddenRunID, ""},
		{http.MethodPost, "/ask/runs/" + hiddenRunID + "/cancel", ""},
	}
	for _, rt := range routes {
		code, hidden := a.do(scoped, rt.method, "/api/v1/databases/search"+rt.suffix, rt.body)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s on search: %d %s, want 404", rt.method, rt.suffix, code, hidden)
		}
		code, unknown := a.do(scoped, rt.method, "/api/v1/databases/nosuch"+rt.suffix, rt.body)
		if code != http.StatusNotFound || unknown != hidden {
			t.Fatalf("%s %s on an unknown database: %d %q, want the same 404 as %q",
				rt.method, rt.suffix, code, unknown, hidden)
		}
	}
	if n := a.runs(t); n != 0 {
		t.Fatalf("%d runs stored after refused requests, want 0", n)
	}
	if n := a.conversations(t); n != 0 {
		t.Fatalf("%d conversations stored after refused requests, want 0", n)
	}
	if body := a.model.lastBody(); body != "" {
		t.Fatalf("the model was called for a refused run: %.200s", body)
	}
}

func TestAskRunsScoped_GrantedDatabaseStartsARun(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	a := newAskScopedAPI(t, src)
	scoped := &auth.User{ID: 7, Role: auth.RoleViewer}
	code, body := a.do(scoped, http.MethodPost, "/api/v1/databases/billing/ask/runs",
		`{"question":"Why is it slow?"}`)
	var started struct {
		RunID string `json:"run_id"`
	}
	if code != http.StatusAccepted || json.Unmarshal([]byte(body), &started) != nil ||
		started.RunID == "" {
		t.Fatalf("scoped run on billing: %d %s, want 202 with a run_id", code, body)
	}
	if state := a.waitRunEnded(t, scoped, "billing", started.RunID); state == "" {
		t.Fatalf("run %s ended with no state", started.RunID)
	}
	if n := a.runs(t); n != 1 {
		t.Fatalf("%d runs stored after one started run, want 1", n)
	}
	// An admin is never scoped: the poll reaches the run lookup, whose 404
	// differs from the hidden-database one.
	path := "/api/v1/databases/search/ask/runs/" + hiddenRunID
	_, hidden := a.do(scoped, http.MethodGet, path, "")
	code, body = a.do(&auth.User{ID: 7, Role: auth.RoleAdmin}, http.MethodGet, path, "")
	if code != http.StatusNotFound || body == hidden || !strings.Contains(body, "not_found") {
		t.Fatalf("admin poll of a missing run on search: %d %s, want the run's own 404 "+
			"(not %s)", code, body, hidden)
	}
}

func TestAskRunsScoped_GrantStoreFailureIsUnavailable(t *testing.T) {
	a := newAskScopedAPI(t, &dbAuthzSource{err: errors.New("connection refused")})
	code, body := a.do(&auth.User{ID: 7, Role: auth.RoleViewer}, http.MethodPost,
		"/api/v1/databases/billing/ask/runs", `{"question":"Why is it slow?"}`)
	if code != http.StatusServiceUnavailable || !bytes.Contains([]byte(body),
		[]byte("grant_store_unavailable")) {
		t.Fatalf("store failure: %d %s, want 503 grant_store_unavailable", code, body)
	}
	if n := a.runs(t); n != 0 {
		t.Fatalf("%d runs stored, want 0", n)
	}
}
