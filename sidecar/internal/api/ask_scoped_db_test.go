package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// apiauth-08c: Ask Sage with grants on. A question to a database outside
// the caller's visible set is answered like an unknown database (404, the
// same body as a database that does not exist) and stores nothing in
// sage.ask_*; the model is never called. No live LLM: a fake server.

type askScopedAPI struct {
	h     http.Handler
	pool  *pgxpool.Pool
	model *askFakeLLM
}

func newAskScopedAPI(t *testing.T, src DatabaseGrantSource) *askScopedAPI {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	if _, err := pool.Exec(ctx, `TRUNCATE sage.ask_conversations, sage.ask_budget_day
		CASCADE`); err != nil {
		t.Fatal(err)
	}
	m := newAskFakeLLM(t)
	reg := ask.NewRegistry()
	for _, db := range []string{"billing", "search"} {
		cfg := config.DefaultConfig().Ask
		cfg.Enabled = true
		svc, err := ask.New(ask.Deps{Database: db, Pool: pool, Config: cfg,
			Settings: config.DefaultConfig(), Proposer: askProposerStub{},
			Model: llm.New(&config.LLMConfig{Enabled: true, Endpoint: m.srv.URL,
				APIKey: "k", Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
				func(string, string, ...any) {})})
		if err != nil {
			t.Fatal(err)
		}
		reg.Set(db, svc)
	}
	mux := http.NewServeMux()
	registerAskRoutes(mux, reg)
	return &askScopedAPI{h: withDatabaseAuthz(&databaseAuthz{grants: src}, mux),
		pool: pool, model: m}
}

func (a *askScopedAPI) post(user *auth.User, db string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/databases/"+db+"/ask",
		bytes.NewReader([]byte(`{"question":"Why is it slow?"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, withUser(req, user))
	return w.Code, w.Body.String()
}

func (a *askScopedAPI) conversations(t *testing.T) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.ask_conversations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAskScoped_HiddenDatabaseIsNotFoundAndStoresNothing(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	a := newAskScopedAPI(t, src)
	scoped := &auth.User{ID: 7, Role: auth.RoleViewer}

	code, hidden := a.post(scoped, "search")
	if code != http.StatusNotFound {
		t.Fatalf("scoped ask on search: %d %s, want 404", code, hidden)
	}
	code, unknown := a.post(scoped, "nosuch")
	if code != http.StatusNotFound || unknown != hidden {
		t.Fatalf("unknown database: %d %q, want the same 404 as %q", code, unknown, hidden)
	}
	if n := a.conversations(t); n != 0 {
		t.Fatalf("%d conversations stored after refused questions, want 0", n)
	}
	if body := a.model.lastBody(); body != "" {
		t.Fatalf("the model was called for a refused question: %.200s", body)
	}
	if code, body := a.post(scoped, "billing"); code != http.StatusOK {
		t.Fatalf("scoped ask on billing: %d %s, want 200", code, body)
	}
	if n := a.conversations(t); n != 1 {
		t.Fatalf("%d conversations after one answered question, want 1", n)
	}
	if code, body := a.post(scoped, "bad%21name"); code != http.StatusBadRequest {
		t.Fatalf("malformed name: %d %s, want 400", code, body)
	}
}

func TestAskScoped_UnscopedAndAdminAskAnyDatabase(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	a := newAskScopedAPI(t, src)
	for _, u := range []*auth.User{{ID: 9, Role: auth.RoleViewer},
		{ID: 7, Role: auth.RoleAdmin}} {
		if code, body := a.post(u, "search"); code != http.StatusOK {
			t.Fatalf("%s %d on search: %d %s, want 200", u.Role, u.ID, code, body)
		}
	}
	if code, _ := a.post(&auth.User{ID: 9, Role: auth.RoleViewer}, "nosuch"); code !=
		http.StatusNotFound {
		t.Fatalf("unscoped unknown database: %d, want 404 as today", code)
	}
}

func TestAskScoped_GrantStoreFailureIsUnavailable(t *testing.T) {
	a := newAskScopedAPI(t, &dbAuthzSource{err: errors.New("connection refused")})
	code, body := a.post(&auth.User{ID: 7, Role: auth.RoleViewer}, "billing")
	if code != http.StatusServiceUnavailable || !bytes.Contains([]byte(body),
		[]byte("grant_store_unavailable")) {
		t.Fatalf("store failure: %d %s, want 503 grant_store_unavailable", code, body)
	}
	if n := a.conversations(t); n != 0 {
		t.Fatalf("%d conversations stored, want 0", n)
	}
}
