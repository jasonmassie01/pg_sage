package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Bulk approve (ux-07b) request rules that need no database: the body is
// checked whole before any item is read, so a bad request decides nothing.
// The per-item decisions on a real database are in bulk_approve_db_test.go.
// No concurrent test here: validation reads only the request.

// poolless is a fleet whose instances have no pool: any item that passes
// validation is refused for its database, never read or run.
func poolless(names ...string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(config.DefaultConfig())
	for _, n := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: n, Status: &fleet.InstanceStatus{}})
	}
	return mgr
}

func bulkItems(n int, db string) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"id": i + 1, "database": db, "card_hash": fmt.Sprintf("h%d", i)}
	}
	return out
}

func bulkCall(t *testing.T, h http.Handler, items any) (int, map[string]any) {
	t.Helper()
	return cardCall(t, h, http.MethodPost, bulkApprovePath, map[string]any{"items": items})
}

func bulkResults(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("no results in %v", body)
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

func wantBadRequest(t *testing.T, code int, body map[string]any, fragment string) {
	t.Helper()
	msg, _ := body["error"].(string)
	if code != http.StatusBadRequest || body["code"] != "invalid_request" ||
		!strings.Contains(msg, fragment) {
		t.Fatalf("got %d %v, want 400 invalid_request containing %q", code, body, fragment)
	}
	if _, ok := body["results"]; ok {
		t.Fatalf("a refused request reported item results: %v", body)
	}
}

func TestBulkApprove_RejectsAnEmptyOrMissingList(t *testing.T) {
	h := actionRouter(t, poolless("orders"), operatorUser())
	code, body := bulkCall(t, h, []map[string]any{})
	wantBadRequest(t, code, body, "at least one item")
	code, body = cardCall(t, h, http.MethodPost, bulkApprovePath, map[string]any{})
	wantBadRequest(t, code, body, "at least one item")
	code, body = cardCall(t, h, http.MethodPost, bulkApprovePath, nil)
	wantBadRequest(t, code, body, "invalid JSON body")
	code, body = cardCall(t, h, http.MethodPost, bulkApprovePath,
		map[string]any{"items": "all"})
	wantBadRequest(t, code, body, "invalid JSON body")
}

func TestBulkApprove_SizeBoundary(t *testing.T) {
	h := actionRouter(t, poolless("orders"), operatorUser())
	code, body := bulkCall(t, h, bulkItems(51, "orders"))
	wantBadRequest(t, code, body, "at most 50 items")
	// 50 is accepted: every item gets its own result (here: refused, as the
	// database has no connection), none is dropped.
	code, body = bulkCall(t, h, bulkItems(50, "orders"))
	if code != http.StatusOK {
		t.Fatalf("50 items: %d %v", code, body)
	}
	results := bulkResults(t, body)
	if len(results) != 50 || body["refused"] != float64(50) || body["approved"] != float64(0) {
		t.Fatalf("50 items: %d results, body %v", len(results), body)
	}
	for i, r := range results {
		if r["id"] != float64(i+1) || r["status"] != "refused" || r["executed"] != false {
			t.Fatalf("result %d = %v", i, r)
		}
	}
}

func TestBulkApprove_RejectsDuplicatesAndMalformedItems(t *testing.T) {
	h := actionRouter(t, poolless("orders", "billing"), operatorUser())
	dup := []map[string]any{
		{"id": 7, "database": "orders", "card_hash": "a"},
		{"id": 7, "database": "orders", "card_hash": "b"},
	}
	code, body := bulkCall(t, h, dup)
	wantBadRequest(t, code, body, `duplicate item 7 on database "orders"`)
	cases := map[string]map[string]any{
		"item 1: id must be a positive":       {"id": 0, "database": "orders", "card_hash": "a"},
		"item 1: card_hash is required":       {"id": 3, "database": "orders", "card_hash": " "},
		"item 1: database is required":        {"id": 3, "card_hash": "a"},
		"item 1: database is required (all)":  {"id": 3, "database": "all", "card_hash": "a"},
		"item 1: id must be a positive (neg)": {"id": -4, "database": "orders", "card_hash": "a"},
	}
	for name, item := range cases {
		fragment := strings.TrimSuffix(strings.SplitN(name, " (", 2)[0], " ")
		code, body := bulkCall(t, h, []map[string]any{item})
		msg, _ := body["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, fragment) {
			t.Errorf("%s: got %d %v", name, code, body)
		}
	}
}

// The same queue id on two databases is two different items.
func TestBulkApprove_SameIDOnTwoDatabasesIsTwoItems(t *testing.T) {
	h := actionRouter(t, poolless("orders", "billing"), operatorUser())
	code, body := bulkCall(t, h, []map[string]any{
		{"id": 7, "database": "orders", "card_hash": "a"},
		{"id": 7, "database": "billing", "card_hash": "a"},
	})
	if code != http.StatusOK {
		t.Fatalf("got %d %v", code, body)
	}
	results := bulkResults(t, body)
	if len(results) != 2 || results[0]["database"] != "orders" ||
		results[1]["database"] != "billing" {
		t.Fatalf("results %v", results)
	}
}

func TestBulkApprove_OneDatabaseFleetNeedsNoDatabase(t *testing.T) {
	h := actionRouter(t, poolless("orders"), operatorUser())
	code, body := bulkCall(t, h, []map[string]any{{"id": 3, "card_hash": "a"}})
	if code != http.StatusOK {
		t.Fatalf("got %d %v", code, body)
	}
	if r := bulkResults(t, body); len(r) != 1 || r[0]["database"] != "orders" {
		t.Fatalf("results %v", r)
	}
}

func TestBulkApprove_UnknownDatabaseRefusesThatItemOnly(t *testing.T) {
	h := actionRouter(t, poolless("orders"), operatorUser())
	code, body := bulkCall(t, h, []map[string]any{
		{"id": 3, "database": "nowhere", "card_hash": "a"},
	})
	if code != http.StatusOK {
		t.Fatalf("got %d %v", code, body)
	}
	r := bulkResults(t, body)[0]
	if r["status"] != "refused" || r["code"] != "not_found" ||
		!strings.Contains(fmt.Sprint(r["reason"]), "database not found") {
		t.Fatalf("result %v", r)
	}
}

func TestBulkApprove_Roles(t *testing.T) {
	mgr := poolless("orders")
	if code, _ := bulkCall(t, actionRouter(t, mgr, viewerUser()),
		bulkItems(1, "orders")); code != http.StatusForbidden {
		t.Fatalf("viewer: %d, want 403", code)
	}
	if code, _ := bulkCall(t, actionRouter(t, mgr, nil),
		bulkItems(1, "orders")); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", code)
	}
}

// A bulk approval spans databases: with database grants on, only an
// operator with no grants (or an admin) may use it (apiauth-08).
func TestBulkApprove_IsAFleetWideWrite(t *testing.T) {
	mux := actionRouter(t, poolless("orders"), operatorUser()).(*http.ServeMux)
	pattern := "POST " + bulkApprovePath
	if got := registeredScopes(mux)[pattern]; got != scopeFleetWide {
		t.Fatalf("scope of %s = %q, want %q", pattern, got, scopeFleetWide)
	}
	if strings.TrimSpace(fleetWideRoutes[pattern]) == "" {
		t.Fatalf("%s is not listed in fleetWideRoutes with its reason", pattern)
	}
}
