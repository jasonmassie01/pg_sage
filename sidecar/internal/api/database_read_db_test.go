package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// apiauth-08b on PostgreSQL: a fleet of two real databases, billing and
// search, each with its own sage schema and one marked finding, action
// and incident. With grants on, a viewer granted billing reads only
// billing through the real router; an unscoped viewer reads both.

func seedReadMarker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string) {
	t.Helper()
	marker := "marker_" + db
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sage.findings (category, severity, object_type, object_identifier,
			title, detail, recommendation, status)
			VALUES ('index_missing', 'warning', 'table', $1, 'finding ' || $1, '{}',
			'look at ' || $1, 'open')`, []any{marker}},
		{`INSERT INTO sage.action_log (action_type, sql_executed, outcome)
			VALUES ('manual', 'ANALYZE ' || $1, 'success')`, []any{marker}},
		{`INSERT INTO sage.incidents (severity, root_cause, source, signal_ids,
			database_name, detected_at)
			VALUES ('warning', 'cause ' || $1, 'deterministic', ARRAY['sig_' || $1], $2,
			now())`, []any{marker, db}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed %s: %v", db, err)
		}
	}
}

func readFleetDB(t *testing.T) *fleet.DatabaseManager {
	t.Helper()
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	for _, db := range []string{"billing", "search"} {
		pool := auditPool(t, "read_"+db)
		seedReadMarker(t, ctx, pool, db)
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: db,
			Config: config.DatabaseConfig{Name: db}, Pool: pool,
			Status: &fleet.InstanceStatus{Connected: true}})
	}
	return mgr
}

func readDBGet(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Body.String()
}

func databaseNames(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		Databases []struct {
			Name string `json:"name"`
		} `json:"databases"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode /api/v1/databases: %v: %s", err, body)
	}
	names := []string{}
	for _, d := range out.Databases {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

var readDBRoutes = []string{"/api/v1/findings?database=all", "/api/v1/actions?database=all",
	"/api/v1/incidents?database=all", "/api/v1/cases?database=all"}

func TestDatabaseReadDB_ScopedViewerSeesOnlyGrantedDatabase(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	adminID := phase2EnsureUser(t, pool, ctx)
	scopedID, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	plainID, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	if _, err := auth.NewDatabaseGrantStore(pool).SetGrant(ctx, auth.GrantChange{
		ActorID: adminID, UserID: scopedID, Database: "billing",
		Role: auth.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	mgr := readFleetDB(t)
	scoped := grantsRouter(t, pool, mgr, &auth.User{ID: scopedID, Role: auth.RoleViewer})
	plain := grantsRouter(t, pool, mgr, &auth.User{ID: plainID, Role: auth.RoleViewer})

	code, body := readDBGet(t, scoped, "/api/v1/databases")
	if got := databaseNames(t, body); code != 200 || strings.Join(got, ",") != "billing" {
		t.Fatalf("scoped /databases: %d %v, want [billing]", code, got)
	}
	code, body = readDBGet(t, plain, "/api/v1/databases")
	if got := databaseNames(t, body); code != 200 || strings.Join(got, ",") != "billing,search" {
		t.Fatalf("unscoped /databases: %d %v, want both", code, got)
	}
	for _, p := range readDBRoutes {
		code, body := readDBGet(t, plain, p)
		if code != 200 || !strings.Contains(body, "marker_billing") ||
			!strings.Contains(body, "marker_search") {
			t.Fatalf("unscoped %s: %d %s, want both markers", p, code, body)
		}
		code, body = readDBGet(t, scoped, p)
		if code != 200 || !strings.Contains(body, "marker_billing") ||
			strings.Contains(body, "marker_search") {
			t.Fatalf("scoped %s: %d %s, want billing only", p, code, body)
		}
	}
}

func TestDatabaseReadDB_HiddenDatabaseAnswersLikeUnknown(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	adminID := phase2EnsureUser(t, pool, ctx)
	scopedID, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	if _, err := auth.NewDatabaseGrantStore(pool).SetGrant(ctx, auth.GrantChange{
		ActorID: adminID, UserID: scopedID, Database: "billing",
		Role: auth.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	mgr := readFleetDB(t)
	h := grantsRouter(t, pool, mgr, &auth.User{ID: scopedID, Role: auth.RoleViewer})
	for _, base := range []string{"/api/v1/findings", "/api/v1/actions",
		"/api/v1/incidents"} {
		hc, hb := readDBGet(t, h, base+"?database=search")
		uc, ub := readDBGet(t, h, base+"?database=nosuch")
		if hc != http.StatusNotFound || hc != uc || hb != ub {
			t.Fatalf("%s: search %d %q vs nosuch %d %q, want identical 404s",
				base, hc, hb, uc, ub)
		}
		mc, _ := readDBGet(t, h, base+"?database=bad%20name")
		if mc != http.StatusBadRequest {
			t.Fatalf("%s: malformed name %d, want 400", base, mc)
		}
	}
	code, body := readDBGet(t, h, "/api/v1/onboarding")
	if code != http.StatusForbidden || !strings.Contains(body, "fleet_scope_required") {
		t.Fatalf("fleet-only read: %d %s, want 403 fleet_scope_required", code, body)
	}
}

func TestDatabaseReadDB_GrantStoreFailureIs503(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	viewerID, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	mgr := readFleetDB(t)
	// A closed control pool stands in for a lost grant store.
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	dead, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	h := grantsRouter(t, dead, mgr, &auth.User{ID: viewerID, Role: auth.RoleViewer})
	code, body := readDBGet(t, h, "/api/v1/findings?database=all")
	if code != http.StatusServiceUnavailable ||
		!strings.Contains(body, "grant_store_unavailable") ||
		strings.Contains(body, "marker_") {
		t.Fatalf("grant store down: %d %s, want 503 with no data", code, body)
	}
}
