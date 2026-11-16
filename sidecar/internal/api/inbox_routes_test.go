package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// GET /api/v1/inbox and /count without a database: argument checks, the
// empty inbox and unread databases. inbox_routes_db_test.go reads real rows.

func inboxMux(mgr *fleet.DatabaseManager, ledgers *earned.Registry,
	grants DatabaseGrantSource) http.Handler {
	mux := http.NewServeMux()
	registerInboxRoutes(mux, mgr, ledgers)
	var authz *databaseAuthz
	if grants != nil {
		authz = &databaseAuthz{grants: grants}
	}
	return withDatabaseAuthz(authz, mux)
}

func inboxGet(t *testing.T, h http.Handler, path string, user *auth.User) (int,
	map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.String()
}

func offlineFleet(names ...string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(config.DefaultConfig())
	for _, n := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: n,
			Config: config.DatabaseConfig{Name: n}, Status: &fleet.InstanceStatus{}})
	}
	return mgr
}

func TestInboxRoutes_RefuseBadArguments(t *testing.T) {
	h := inboxMux(offlineFleet("orders"), nil, nil)
	for _, c := range []struct {
		path, want string
		code       int
	}{
		{"/api/v1/inbox?database=../x", "invalid database", 400},
		{"/api/v1/inbox?database=nope", "database not found", 404},
		{"/api/v1/inbox/count?database=nope", "database not found", 404},
		{"/api/v1/inbox?limit=0", "from 1 to 10000", 400},
		{"/api/v1/inbox?limit=-1", "from 1 to 10000", 400},
		{"/api/v1/inbox?limit=10001", "from 1 to 10000", 400},
		{"/api/v1/inbox?limit=ten", "from 1 to 10000", 400},
		{"/api/v1/inbox?kinds=facts", "known kinds", 400},
	} {
		code, _, body := inboxGet(t, h, c.path, testViewerUser())
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s = %d %s, want %d with %q", c.path, code, body, c.code, c.want)
		}
	}
	if code, _, body := inboxGet(t, h, "/api/v1/inbox?limit=10000&kinds=fact",
		testViewerUser()); code != 200 {
		t.Fatalf("limit 10000 is inclusive: %d %s", code, body)
	}
}

func TestInboxRoutes_EmptyFleetIsAnEmptyListNotNull(t *testing.T) {
	h := inboxMux(fleet.NewManager(config.DefaultConfig()), nil, nil)
	code, _, body := inboxGet(t, h, "/api/v1/inbox", testViewerUser())
	if code != 200 || !strings.Contains(body, `"items":[]`) ||
		!strings.Contains(body, `"errors":[]`) || !strings.Contains(body, `"total":0`) ||
		!strings.Contains(body, `"lower_bound":false`) {
		t.Fatalf("empty inbox = %d %s", code, body)
	}
	code, out, _ := inboxGet(t, h, "/api/v1/inbox/count", testViewerUser())
	counts, _ := out["counts_by_kind"].(map[string]any)
	if code != 200 || out["total"] != float64(0) || len(counts) != 7 || out["items"] != nil {
		t.Fatalf("empty count = %d %v", code, out)
	}
}

func TestInboxRoutes_AnUnreadDatabaseMakesTheCountALowerBound(t *testing.T) {
	h := inboxMux(offlineFleet("orders", "billing"), nil, nil)
	code, out, body := inboxGet(t, h, "/api/v1/inbox/count", testOperatorUser())
	errs, _ := out["errors"].([]any)
	if code != 200 || out["lower_bound"] != true || out["total"] != float64(0) ||
		len(errs) != 12 {
		t.Fatalf("count = %d %s, want a lower bound with 6 kinds x 2 unread databases",
			code, body)
	}
	first := errs[0].(map[string]any)
	if first["error"] != "not connected" || first["source"] == "" {
		t.Fatalf("error %v, want a named not-connected source", first)
	}
	code, out, _ = inboxGet(t, h, "/api/v1/inbox?database=billing", testOperatorUser())
	if errs, _ := out["errors"].([]any); code != 200 || len(errs) != 6 {
		t.Fatalf("one database = %d %v, want its six unread sources", code, out)
	}
}

func TestInboxRoutes_AFailingLedgerIsNamed(t *testing.T) {
	st, err := earned.NewPostgresStore(unreachablePool(t),
		"6f1c2a4e-0b7d-4c1e-9a55-1f2e3d4c5b6a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := earned.NewService(st, earned.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	reg := earned.NewRegistry(false)
	reg.Register("orders", earned.RegistryEntry{Service: svc})
	h := inboxMux(offlineFleet("orders"), reg, nil)
	_, out, body := inboxGet(t, h, "/api/v1/inbox/count?kinds=autonomy_promotion",
		testViewerUser())
	errs, _ := out["errors"].([]any)
	if out["lower_bound"] != true || len(errs) != 1 ||
		errs[0].(map[string]any)["source"] != "autonomy_promotion" ||
		errs[0].(map[string]any)["database"] != "orders" {
		t.Fatalf("count %s, want the ledger named as unread", body)
	}
}

func TestInboxRoutes_AScopedUserNeverSeesAHiddenDatabase(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(2, map[string]string{"orders": auth.RoleOperator})
	h := inboxMux(offlineFleet("orders", "billing"), nil, src)
	user := dbOperator(2)
	code, out, _ := inboxGet(t, h, "/api/v1/inbox/count", user)
	errs, _ := out["errors"].([]any)
	if code != 200 || len(errs) != 6 {
		t.Fatalf("scoped count = %d %v, want only orders' six sources", code, out)
	}
	for _, e := range errs {
		if e.(map[string]any)["database"] != "orders" {
			t.Fatalf("a hidden database leaked: %v", e)
		}
	}
	if code, _, body := inboxGet(t, h, "/api/v1/inbox?database=billing",
		user); code != 404 || !strings.Contains(body, "database not found") {
		t.Fatalf("hidden database = %d %s, want 404 like an unknown one", code, body)
	}
	src.err = context.DeadlineExceeded
	if code, _, _ := inboxGet(t, h, "/api/v1/inbox", user); code != 503 {
		t.Fatalf("grant store down = %d, want 503 (never allowed)", code)
	}
}

func TestInboxMCP_RestrictsToTheCallersDatabases(t *testing.T) {
	b := InboxMCP(offlineFleet("orders", "billing"), nil)
	got, err := b.Inbox(context.Background(), mcp.InboxRequest{Databases: []string{"orders"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "billing") || !strings.Contains(string(raw), "orders") {
		t.Fatalf("restricted inbox %s leaks billing", raw)
	}
	if _, err := b.Inbox(context.Background(), mcp.InboxRequest{Database: "billing",
		Databases: []string{"orders"}}); err != mcp.ErrInboxDatabaseNotFound {
		t.Fatalf("hidden database: %v, want ErrInboxDatabaseNotFound", err)
	}
	if _, err := b.Inbox(context.Background(), mcp.InboxRequest{Database: "nope"}); err !=
		mcp.ErrInboxDatabaseNotFound {
		t.Fatalf("unknown database: %v", err)
	}
	if _, err := b.Inbox(context.Background(), mcp.InboxRequest{
		Kinds: []string{"bogus"}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	nilFleet := InboxMCP(nil, nil)
	got, err = nilFleet.Inbox(context.Background(), mcp.InboxRequest{})
	if raw, _ := json.Marshal(got); err != nil || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("no fleet: %s %v", raw, err)
	}
}
