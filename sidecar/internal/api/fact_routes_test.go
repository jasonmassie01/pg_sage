package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Roadmap 2.3: the Facts API lists a database's facts, lets an operator
// declare one (confirmed by the declaration), confirm, reject or expire a
// proposal, and answers which facts bind given objects (for findings and
// approval cards).

type factAPI struct {
	pool *pgxpool.Pool
	mgr  *fleet.DatabaseManager
	mux  *http.ServeMux
}

func newFactAPI(t *testing.T) *factAPI {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	clean := func() { _, _ = pool.Exec(context.Background(), "DELETE FROM sage.facts") }
	clean()
	t.Cleanup(clean)
	_ = ctx
	f := &factAPI{pool: pool, mgr: phase2MgrWithPool(pool), mux: http.NewServeMux()}
	registerFactRoutes(f.mux, f.mgr)
	return f
}

func (f *factAPI) do(t *testing.T, user *auth.User, method, path string,
	body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func viewerUser() *auth.User {
	return &auth.User{ID: 3, Email: "viewer@test.com", Role: auth.RoleViewer}
}

func (f *factAPI) propose(t *testing.T, subject string) facts.Fact {
	t.Helper()
	fact, _, err := facts.NewStore(f.pool).Propose(context.Background(), facts.Proposal{
		Type: facts.TypeTestFixture, Kind: facts.KindSchema, Subject: subject,
		Source: facts.SourceDetector, Evidence: []facts.Citation{{Kind: "catalog",
			Ref: "schemas:" + subject, Detail: "idle copies"}}})
	if err != nil {
		t.Fatal(err)
	}
	return fact
}

func TestFactRoutesRoles(t *testing.T) {
	f := newFactAPI(t)
	if code, _ := f.do(t, nil, "GET", "/api/v1/facts?database=testdb", nil); code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, body := f.do(t, viewerUser(), "GET", "/api/v1/facts?database=testdb",
		nil); code != http.StatusOK || body["facts"] == nil {
		t.Fatalf("viewer list: %d %v", code, body)
	}
	p := f.propose(t, "test_roles_*")
	if code, _ := f.do(t, viewerUser(), "POST",
		fmt.Sprintf("/api/v1/facts/%d/confirm?database=testdb", p.ID), nil); code !=
		http.StatusForbidden {
		t.Fatalf("a viewer confirmed a fact: %d", code)
	}
	if code, _ := f.do(t, viewerUser(), "POST", "/api/v1/facts?database=testdb",
		map[string]any{"type": "test_fixture", "subject_kind": "schema",
			"subject": "qa_*"}); code != http.StatusForbidden {
		t.Fatalf("a viewer declared a fact: %d", code)
	}
}

func TestFactRoutesDeclareConfirmRejectExpire(t *testing.T) {
	f := newFactAPI(t)
	code, body := f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb",
		map[string]any{"type": "slot_consumer", "subject_kind": "slot",
			"subject": "cdc_orders", "value": map[string]string{"consumer": "debezium"},
			"note": "orders CDC to the warehouse"})
	fact, _ := body["fact"].(map[string]any)
	if code != http.StatusCreated || fact["status"] != "confirmed" ||
		fact["decided_by"] != "operator@test.com" || fact["source"] != "operator" {
		t.Fatalf("declare: %d %v", code, body)
	}
	code, body = f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb",
		map[string]any{"type": "test_fixture", "subject_kind": "schema", "subject": "s*"})
	if code != http.StatusBadRequest || body["code"] != "protected_subject" {
		t.Fatalf("protected subject: %d %v", code, body)
	}
	code, body = f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb",
		map[string]any{"type": "test_fixture", "subject_kind": "slot", "subject": "x"})
	if code != http.StatusBadRequest || body["code"] != "invalid_fact" {
		t.Fatalf("invalid kind: %d %v", code, body)
	}
	p := f.propose(t, "test_flow_*")
	path := func(id int64, verb string) string {
		return fmt.Sprintf("/api/v1/facts/%d/%s?database=testdb", id, verb)
	}
	code, body = f.do(t, testAdminUser(), "POST", path(p.ID, "confirm"),
		map[string]string{"note": "CI leaks these"})
	if fact, _ := body["fact"].(map[string]any); code != http.StatusOK ||
		fact["status"] != "confirmed" || fact["decided_by"] != "admin@test.com" {
		t.Fatalf("confirm: %d %v", code, body)
	}
	code, body = f.do(t, testOperatorUser(), "POST", path(p.ID, "reject"), nil)
	if fact, _ := body["fact"].(map[string]any); code != http.StatusOK ||
		fact["status"] != "rejected" {
		t.Fatalf("reject: %d %v", code, body)
	}
	code, body = f.do(t, testOperatorUser(), "POST", path(p.ID, "expire"),
		map[string]string{"note": "fixtures moved"})
	if code != http.StatusConflict || body["code"] != "invalid_transition" {
		t.Fatalf("a rejected fact cannot expire: %d %v", code, body)
	}
	if code, body = f.do(t, testOperatorUser(), "POST", path(987654, "confirm"),
		nil); code != http.StatusNotFound {
		t.Fatalf("unknown fact: %d %v", code, body)
	}
	if code, _ = f.do(t, testOperatorUser(), "POST", "/api/v1/facts/abc/confirm?database=testdb",
		nil); code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ = f.do(t, testOperatorUser(), "POST", path(p.ID, "confirm")+"x",
		nil); code != http.StatusNotFound && code != http.StatusBadRequest {
		t.Fatalf("unknown database: %d", code)
	}
}

func TestFactRoutesListAndMatch(t *testing.T) {
	f := newFactAPI(t)
	store := facts.NewStore(f.pool)
	if _, err := store.Declare(context.Background(), facts.Proposal{
		Type: facts.TypeAppMigrations, Kind: facts.KindTable, Subject: "public.orders",
		Source: facts.SourceOperator}, "op@test.com", ""); err != nil {
		t.Fatal(err)
	}
	f.propose(t, "test_match_*")
	code, body := f.do(t, viewerUser(), "GET",
		"/api/v1/facts?database=testdb&status=proposed", nil)
	list, _ := body["facts"].([]any)
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("proposed list: %d %v", code, body)
	}
	if code, body = f.do(t, viewerUser(), "GET", "/api/v1/facts?database=testdb&status=maybe",
		nil); code != http.StatusBadRequest {
		t.Fatalf("bad status: %d %v", code, body)
	}
	code, body = f.do(t, viewerUser(), "GET", "/api/v1/facts/match?database=testdb"+
		"&object=public.orders&object=test_match_a1.t&object=other.x", nil)
	objects, _ := body["objects"].(map[string]any)
	orders, _ := objects["public.orders"].(map[string]any)
	fixture, _ := objects["test_match_a1.t"].(map[string]any)
	other, _ := objects["other.x"].(map[string]any)
	if code != http.StatusOK || len(asList(orders["confirmed"])) != 1 ||
		len(asList(fixture["proposed"])) != 1 || len(asList(fixture["confirmed"])) != 0 ||
		len(asList(other["confirmed"])) != 0 {
		t.Fatalf("match: %d %v", code, body)
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
