package decommission

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func dropBody(digest, confirm string) string {
	return fmt.Sprintf(`{"inventory_sha256":%q,"confirm":%q}`, digest, confirm)
}

func TestDropHandler_RefusesBadRequests(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_http_bad")
	ackAllBut(t, ctx, pool)
	digest := mustBuild(t, ctx, pool).InventorySHA256
	h := NewHandlers(pool, fixedActor(), t.Logf)
	cases := []struct {
		name, body string
		code       int
		contains   string
	}{
		{"unknown field", `{"inventory_sha256":"` + digest + `","confirm":"` +
			DropConfirmPhrase + `","cascade":true}`, http.StatusBadRequest, "invalid"},
		{"wrong confirm", dropBody(digest, "yes"), http.StatusBadRequest, DropConfirmPhrase},
		{"bad digest", dropBody("abc", DropConfirmPhrase), http.StatusBadRequest,
			"inventory_sha256"},
		{"bad json", `{"confirm":`, http.StatusBadRequest, "invalid"},
		{"trailing value", dropBody(digest, DropConfirmPhrase) + `{"cascade":true}`,
			http.StatusBadRequest, "invalid"},
		{"trailing brace", dropBody(digest, DropConfirmPhrase) + `}`,
			http.StatusBadRequest, "invalid"},
		{"empty", ``, http.StatusBadRequest, "invalid"},
	}
	for _, c := range cases {
		rec := serve(h.Drop, http.MethodPost, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, rec.Code, rec.Body,
				c.code, c.contains)
		}
	}
	if n := legacyCount(t, ctx, pool); n != 27 {
		t.Fatalf("a refused request dropped tables: %d remain", n)
	}
}

// CHECK-03: with an unacknowledged item the route returns 409 and keeps 27.
func TestDropHandler_ConflictsKeepTheTables(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_http_409")
	ackAllBut(t, ctx, pool, "provider_resource:d-op")
	h := NewHandlers(pool, fixedActor(), t.Logf)
	digest := mustBuild(t, ctx, pool).InventorySHA256
	rec := serve(h.Drop, http.MethodPost, dropBody(digest, DropConfirmPhrase))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "unacknowledged") {
		t.Fatalf("unacknowledged drop = %d %s, want 409", rec.Code, rec.Body)
	}
	ackAllBut(t, ctx, pool)
	rec = serve(h.Drop, http.MethodPost, dropBody(strings.Repeat("0", 64), DropConfirmPhrase))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stale") {
		t.Fatalf("stale drop = %d %s, want 409", rec.Code, rec.Body)
	}
	mustExec(t, ctx, pool, `CREATE VIEW public.cost_view AS
		SELECT * FROM sage.agent_db_cost_samples`)
	rec = serve(h.Drop, http.MethodPost, dropBody(digest, DropConfirmPhrase))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "cost_view") {
		t.Fatalf("dependent view drop = %d %s, want 409 naming the view", rec.Code, rec.Body)
	}
	if n := legacyCount(t, ctx, pool); n != 27 {
		t.Fatalf("CHECK-03: %d legacy tables remain, want 27", n)
	}
}

func TestDropHandler_DropsAndListsTheTables(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_http_ok")
	digest := mustBuild(t, ctx, pool).InventorySHA256
	ackAllBut(t, ctx, pool)
	h := NewHandlers(pool, fixedActor(), t.Logf)
	rec := serve(h.Drop, http.MethodPost, dropBody(digest, DropConfirmPhrase))
	if rec.Code != http.StatusOK {
		t.Fatalf("drop = %d %s", rec.Code, rec.Body)
	}
	var res DropResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Dropped) != 27 || res.Dropped[0] != LegacyTables[0] {
		t.Fatalf("dropped = %v", res.Dropped)
	}
	rec = serve(h.Drop, http.MethodPost,
		dropBody(mustBuild(t, ctx, pool).InventorySHA256, DropConfirmPhrase))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dropped":[]`) {
		t.Fatalf("repeat drop = %d %s, want 200 with an empty list", rec.Code, rec.Body)
	}
	if n := dropAuditCount(t, ctx, pool); n != 27 {
		t.Fatalf("audit rows = %d, want 27", n)
	}
}

func TestDropHandler_UnavailableAnonymousAndFailingStores(t *testing.T) {
	body := dropBody(strings.Repeat("ab", 32), DropConfirmPhrase)
	if rec := serve(NewHandlers(nil, fixedActor(), t.Logf).Drop, http.MethodPost,
		body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil pool drop = %d", rec.Code)
	}
	pool, _ := legacyDB(t, "decom_drop_http_closed")
	anon := NewHandlers(pool, func(*http.Request) string { return "" }, t.Logf)
	if rec := serve(anon.Drop, http.MethodPost, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous drop = %d", rec.Code)
	}
	if rec := serve(NewHandlers(pool, fixedActor(), t.Logf).Drop, http.MethodPost,
		body); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "config_audit") {
		t.Fatalf("drop without the audit table = %d %s, want 503", rec.Code, rec.Body)
	}
	pool.Close()
	var logged []string
	h := NewHandlers(pool, fixedActor(), func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	})
	rec := serve(h.Drop, http.MethodPost, body)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(),
		"closed") || len(logged) == 0 {
		t.Fatalf("closed pool drop = %d %s, logged %v; want 500, logged not echoed",
			rec.Code, rec.Body, logged)
	}
}
