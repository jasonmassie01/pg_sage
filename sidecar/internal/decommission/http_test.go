package decommission

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, InventoryPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func fixedActor() func(*http.Request) string {
	return func(*http.Request) string { return "admin@example.test" }
}

func TestInventoryHandler_ReturnsTheInventoryAsJSON(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_http")
	seedEstate(t, ctx, pool)
	h := NewHandlers(pool, fixedActor(), t.Logf)
	rec := serve(h.Inventory, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	var inv Inventory
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !inv.LegacyTables || len(inv.Items) != 8 || inv.Spec != SpecRef {
		t.Fatalf("inventory = legacy %v, %d items, spec %q", inv.LegacyTables,
			len(inv.Items), inv.Spec)
	}
	if strings.Contains(rec.Body.String(), "inline-secret-value") {
		t.Fatal("the response leaked a secret value")
	}
}

func TestAckHandler_StatusCodes(t *testing.T) {
	pool, ctx := freshDB(t, "decom_http_ack")
	bootstrapped(t, ctx, pool)
	applyLegacySchema(t, ctx, pool)
	seedEstate(t, ctx, pool)
	h := NewHandlers(pool, fixedActor(), t.Logf)
	cases := []struct {
		name, body string
		code       int
		contains   string
	}{
		{"ok", `{"acknowledged_resources":["provider_resource:d-op"],"exported":true}`,
			http.StatusOK, `"acknowledged":["provider_resource:d-op"]`},
		{"again", `{"acknowledged_resources":["provider_resource:d-op"],"exported":true}`,
			http.StatusOK, `"already_acknowledged":["provider_resource:d-op"]`},
		{"not exported", `{"acknowledged_resources":["provider_resource:d-op"]}`,
			http.StatusBadRequest, "exported"},
		{"empty", `{"acknowledged_resources":[],"exported":true}`,
			http.StatusBadRequest, "acknowledged_resources"},
		{"unknown", `{"acknowledged_resources":["nope"],"exported":true}`,
			http.StatusBadRequest, `"unknown_resources":["nope"]`},
		{"bad json", `{"acknowledged_resources":`, http.StatusBadRequest, "invalid"},
		{"extra field", `{"acknowledged_resources":["a"],"exported":true,"force":true}`,
			http.StatusBadRequest, "invalid"},
	}
	for _, c := range cases {
		rec := serve(h.Ack, http.MethodPost, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s: %d %s, want %d containing %s", c.name, rec.Code, rec.Body,
				c.code, c.contains)
		}
	}
	var by string
	if err := pool.QueryRow(ctx, `SELECT acknowledged_by FROM sage.agentdb_decommission
		WHERE resource_id = 'provider_resource:d-op'`).Scan(&by); err != nil ||
		by != "admin@example.test" {
		t.Fatalf("actor = %q, %v", by, err)
	}
}

func TestHandlers_UnavailableAndFailingStores(t *testing.T) {
	h := NewHandlers(nil, fixedActor(), t.Logf)
	if rec := serve(h.Inventory, http.MethodGet, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil pool GET = %d", rec.Code)
	}
	if rec := serve(h.Ack, http.MethodPost, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil pool POST = %d", rec.Code)
	}
	pool, _ := legacyDB(t, "decom_http_closed")
	pool.Close()
	var logged []string
	h = NewHandlers(pool, fixedActor(), func(f string, a ...any) { logged = append(logged, f) })
	rec := serve(h.Inventory, http.MethodGet, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("closed pool GET = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "closed") || len(logged) == 0 {
		t.Fatalf("a store failure must be logged, not echoed: body %s, logged %v",
			rec.Body, logged)
	}
	noActor := NewHandlers(pool, func(*http.Request) string { return "" }, t.Logf)
	rec = serve(noActor.Ack, http.MethodPost,
		`{"acknowledged_resources":["a"],"exported":true}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous ack = %d", rec.Code)
	}
}
