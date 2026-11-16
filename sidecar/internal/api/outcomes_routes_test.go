package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// GET /api/v1/outcomes without a database: argument checks, unread
// databases and per-database grants. outcomes_routes_db_test.go reads rows.

func outcomesMux(mgr *fleet.DatabaseManager, grants DatabaseGrantSource) http.Handler {
	mux := http.NewServeMux()
	registerOutcomesRoutes(mux, mgr)
	var authz *databaseAuthz
	if grants != nil {
		authz = &databaseAuthz{grants: grants}
	}
	return withDatabaseAuthz(authz, mux)
}

func TestOutcomesRoute_RefusesBadArguments(t *testing.T) {
	h := outcomesMux(offlineFleet("orders"), nil)
	for _, c := range []struct {
		path, want string
		code       int
	}{
		{"/api/v1/outcomes?since=abc", "9600h", 400},
		{"/api/v1/outcomes?since=-1h", "9600h", 400},
		{"/api/v1/outcomes?since=9601h", "9600h", 400},
		{"/api/v1/outcomes?verdict=bogus", "no_change", 400},
		{"/api/v1/outcomes?limit=0", "1 to 10000", 400},
		{"/api/v1/outcomes?limit=10001", "1 to 10000", 400},
		{"/api/v1/outcomes?database=../x", "invalid database", 400},
		{"/api/v1/outcomes?database=nope", "database not found", 404},
	} {
		code, _, body := inboxGet(t, h, c.path, testViewerUser())
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s = %d %s, want %d and %q", c.path, code, body, c.code, c.want)
		}
	}
	if code, _, body := inboxGet(t, h, "/api/v1/outcomes?since=9600h",
		testViewerUser()); code != 200 {
		t.Fatalf("since=9600h = %d %s, want 200 (the retention bound, inclusive)", code, body)
	}
}

func TestOutcomesRoute_AnUnreadDatabaseIsNamedNotEmpty(t *testing.T) {
	h := outcomesMux(offlineFleet("orders"), nil)
	code, out, body := inboxGet(t, h, "/api/v1/outcomes", testViewerUser())
	rows, _ := out["rows"].([]any)
	errs, _ := out["errors"].([]any)
	summary, _ := out["summary"].(map[string]any)
	if code != 200 || rows == nil || len(rows) != 0 || len(errs) != 1 || summary == nil {
		t.Fatalf("offline = %d %s", code, body)
	}
	e := errs[0].(map[string]any)
	if e["database"] != "orders" || !strings.Contains(e["error"].(string),
		"read outcomes: orders: ") {
		t.Fatalf("error %v, want read outcomes: orders: ...", e)
	}
	if v, set := summary["measured_share"]; !set || v != nil {
		t.Fatalf("summary %v, want measured_share null", summary)
	}
	if out["since"] != "24h" {
		t.Fatalf("since = %v, want the default 24h", out["since"])
	}
}

func TestOutcomesRoute_AScopedUserNeverSeesAHiddenDatabase(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(2, map[string]string{"orders": auth.RoleOperator})
	h := outcomesMux(offlineFleet("orders", "billing"), src)
	code, out, body := inboxGet(t, h, "/api/v1/outcomes", dbOperator(2))
	if code != 200 || strings.Contains(body, "billing") || !strings.Contains(body, "orders") {
		t.Fatalf("scoped = %d %v", code, out)
	}
	if code, _, body := inboxGet(t, h, "/api/v1/outcomes?database=billing",
		dbOperator(2)); code != 404 || !strings.Contains(body, "database not found") {
		t.Fatalf("hidden database = %d %s, want 404 like an unknown one", code, body)
	}
}

func TestOutcomesMCP_RestrictsToTheCallersDatabases(t *testing.T) {
	b := OutcomesMCP(offlineFleet("orders", "billing"))
	got, err := b.Outcomes(context.Background(), mcp.OutcomesRequest{Since: 24 * time.Hour,
		Limit: 10, Databases: []string{"orders"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "billing") || !strings.Contains(string(raw), "orders") {
		t.Fatalf("restricted ledger %s leaks billing", raw)
	}
	if _, err := b.Outcomes(context.Background(), mcp.OutcomesRequest{Database: "billing",
		Databases: []string{"orders"}}); err != mcp.ErrOutcomesDatabaseNotFound {
		t.Fatalf("hidden database: %v, want ErrOutcomesDatabaseNotFound", err)
	}
	if _, err := b.Outcomes(context.Background(), mcp.OutcomesRequest{
		Database: "nope"}); err != mcp.ErrOutcomesDatabaseNotFound {
		t.Fatalf("unknown database: %v", err)
	}
	got, err = OutcomesMCP(nil).Outcomes(context.Background(), mcp.OutcomesRequest{})
	if raw, _ := json.Marshal(got); err != nil || !strings.Contains(string(raw), `"rows":[]`) {
		t.Fatalf("no fleet: %s %v", raw, err)
	}
}
