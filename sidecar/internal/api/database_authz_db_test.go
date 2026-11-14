package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// apiauth-08 on PostgreSQL: the admin grant editor writes
// sage.user_database_grants and the auth audit chain; the router reads the
// same table on every scoped write; a ChatOps approval by a scoped user on
// another database is refused, audited, and leaves the card usable.

func grantsFleet(names ...string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, n := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: n,
			Config: config.DatabaseConfig{Name: n}})
	}
	return mgr
}

func serveGrants(pool *pgxpool.Pool, mgr *fleet.DatabaseManager, method, path, body string,
	caller *auth.User) (int, map[string]any) {
	mux := http.NewServeMux()
	registerDatabaseGrantRoutes(mux, pool, mgr, true, false)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return serveJSON(mux, withUser(req, caller))
}

func authAuditCount(t *testing.T, pool *pgxpool.Pool, event string, target int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = $2`, event, target).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", event, err)
	}
	return n
}

func TestDatabaseAuthzDB_GrantEditor(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	admin := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	op, _ := statusTestUser(t, ctx, pool, auth.RoleOperator)
	mgr := grantsFleet("orders", "billing")
	base := fmt.Sprintf("/api/v1/users/%d/database-grants", op)

	code, body := serveGrants(pool, mgr, "PUT", base+"/orders", `{"role":"operator"}`, admin)
	if code != http.StatusOK || body["database"] != "orders" || body["role"] != "operator" {
		t.Fatalf("grant orders: %d %v", code, body)
	}
	requireGrantEditorRejects(t, pool, mgr, base, admin)
	code, body = serveGrants(pool, mgr, "PUT", "/api/v1/users/987654/database-grants/orders",
		`{"role":"viewer"}`, admin)
	if code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("unknown user: %d %v, want 404", code, body)
	}
	requireGrantEditorListAndRevoke(t, pool, mgr, base, admin, op)
}

func requireGrantEditorRejects(t *testing.T, pool *pgxpool.Pool, mgr *fleet.DatabaseManager,
	base string, admin *auth.User) {
	t.Helper()
	bad := []struct{ name, path, body, code string }{
		{"unknown database", base + "/payroll", `{"role":"operator"}`, "unknown_database"},
		{"admin role", base + "/orders", `{"role":"admin"}`, "invalid_role"},
		{"empty role", base + "/orders", `{}`, "invalid_role"},
		{"all databases", base + "/all", `{"role":"viewer"}`, "unknown_database"},
		{"bad JSON", base + "/orders", `{"role":`, "invalid_request"},
		{"bad user id", "/api/v1/users/x/database-grants/orders", `{"role":"viewer"}`,
			"invalid_request"},
	}
	for _, tc := range bad {
		code, body := serveGrants(pool, mgr, "PUT", tc.path, tc.body, admin)
		if code != http.StatusBadRequest || body["code"] != tc.code {
			t.Errorf("%s: %d %v, want 400 %s", tc.name, code, body, tc.code)
		}
	}
}

func requireGrantEditorListAndRevoke(t *testing.T, pool *pgxpool.Pool,
	mgr *fleet.DatabaseManager, base string, admin *auth.User, op int) {
	t.Helper()
	code, body := serveGrants(pool, mgr, "GET", base, "", admin)
	grants, _ := body["grants"].([]any)
	if code != http.StatusOK || len(grants) != 1 || body["enforced"] != true ||
		body["scoped"] != true {
		t.Fatalf("list: %d %v", code, body)
	}
	if g := grants[0].(map[string]any); g["database"] != "orders" ||
		g["role"] != "operator" || int(g["granted_by"].(float64)) != admin.ID {
		t.Fatalf("listed grant %v", g)
	}

	code, body = serveGrants(pool, mgr, "DELETE", base+"/orders", "", admin)
	if code != http.StatusOK || body["revoked"] != true {
		t.Fatalf("revoke: %d %v", code, body)
	}
	code, body = serveGrants(pool, mgr, "DELETE", base+"/orders", "", admin)
	if code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("second revoke: %d %v, want 404", code, body)
	}
	if n := authAuditCount(t, pool, auth.AuditDatabaseGrantSet, op); n != 1 {
		t.Fatalf("%d grant-set audit rows, want 1 (refused writes are not audited)", n)
	}
	if n := authAuditCount(t, pool, auth.AuditDatabaseGrantRevoked, op); n != 1 {
		t.Fatalf("%d grant-revoked audit rows, want 1", n)
	}
}

// grantsRouter is the fleet router with grants on, the real grant store,
// and the given user signed in.
func grantsRouter(t *testing.T, pool *pgxpool.Pool, mgr *fleet.DatabaseManager,
	user *auth.User) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode, cfg.API.DatabaseGrants = "fleet", true
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, user))
		})
	}
	return newTestRouter(mgr, cfg, pool, &ActionDeps{Fleet: mgr}, nil, nil, &RuntimeDeps{},
		inject)
}

func TestDatabaseAuthzDB_RouterStateTransitions(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	adminID := phase2EnsureUser(t, pool, ctx)
	opID, _ := statusTestUser(t, ctx, pool, auth.RoleOperator)
	mgr := grantsFleet("orders", "billing")
	h := grantsRouter(t, pool, mgr, &auth.User{ID: opID, Role: auth.RoleOperator})
	grants := auth.NewDatabaseGrantStore(pool)
	decide := func(db string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/v1/managed-changes/3/approve?database="+db,
			strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		return serveJSON(h, req)
	}
	reachedHandler := func(label string, code int, body map[string]any) {
		t.Helper()
		// The instance has no pool: the handler itself answers 404.
		if code != http.StatusNotFound || body["error"] != "database not found" {
			t.Fatalf("%s: %d %v, want the handler's 404", label, code, body)
		}
	}
	code, body := decide("billing")
	reachedHandler("no grants: global operator on billing", code, body)

	for _, g := range []auth.GrantChange{
		{ActorID: adminID, UserID: opID, Database: "orders", Role: auth.RoleOperator},
		{ActorID: adminID, UserID: opID, Database: "billing", Role: auth.RoleViewer},
	} {
		if _, err := grants.SetGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	code, body = decide("orders")
	reachedHandler("operator grant on orders", code, body)
	code, body = decide("billing")
	requireForbidden(t, "viewer grant on billing", code, body)

	if err := grants.RevokeGrant(ctx, auth.GrantChange{ActorID: adminID, UserID: opID,
		Database: "orders"}); err != nil {
		t.Fatal(err)
	}
	code, body = decide("orders")
	requireForbidden(t, "after revoking orders", code, body)
}

func TestDatabaseAuthzDB_ChatOpsScopedUserRefusedAndAudited(t *testing.T) {
	cf := newChatCardFixture(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.Mode, cfg.API.DatabaseGrants = "fleet", true
	cf.h = newTestRouter(cf.mgr, cfg, cf.pool, &ActionDeps{Fleet: cf.mgr}, nil, nil,
		&RuntimeDeps{})
	grants := auth.NewDatabaseGrantStore(cf.pool)
	// The operator is scoped to another database, so the "orders" card is
	// out of reach.
	if _, err := grants.SetGrant(ctx, auth.GrantChange{ActorID: cf.operator,
		UserID: cf.operator, Database: "billing", Role: auth.RoleOperator}); err != nil {
		t.Fatal(err)
	}
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	requireForbidden(t, "chatops approve on orders", code, body)
	requireChatRefusalAudited(t, cf)

	// Granting the database makes the same card usable: the refusal did
	// not consume it.
	if _, err := grants.SetGrant(ctx, auth.GrantChange{ActorID: cf.operator,
		UserID: cf.operator, Database: "orders", Role: auth.RoleOperator}); err != nil {
		t.Fatal(err)
	}
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["executed"] != true {
		t.Fatalf("approve after the grant: %d %v, want 200 executed", code, body)
	}
}

// requireChatRefusalAudited checks the refusal's audit row, that the queue
// item is untouched and that the chat was told why.
func requireChatRefusalAudited(t *testing.T, cf *chatCardFixture) {
	t.Helper()
	ctx := context.Background()
	if n := authAuditCount(t, cf.pool, auth.AuditDatabaseForbidden, cf.operator); n != 1 {
		t.Fatalf("%d database_forbidden audit rows, want 1", n)
	}
	var detail string
	if err := cf.pool.QueryRow(ctx, `SELECT detail::text FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = $2`, auth.AuditDatabaseForbidden,
		cf.operator).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, `"database": "orders"`) ||
		!strings.Contains(detail, `"surface": "chatops"`) {
		t.Fatalf("audit detail %s, want database orders and surface chatops", detail)
	}
	a, err := store.NewActionStore(cf.pool).GetByID(ctx, cf.queueID)
	if err != nil || a.Status != "pending" {
		t.Fatalf("queue item after a refused approval: %+v (%v), want pending", a, err)
	}
	if got := cf.replies.last(); !strings.Contains(got, "orders") {
		t.Fatalf("chat reply %q does not explain the refusal", got)
	}
}

// A database leaves the fleet while a user still holds a grant on it: the
// admin must still be able to revoke that grant (it keeps the user
// scoped), though a new grant on it is refused.
func TestDatabaseAuthzDB_RevokeGrantOnRemovedDatabase(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	admin := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	op, _ := statusTestUser(t, ctx, pool, auth.RoleOperator)
	base := fmt.Sprintf("/api/v1/users/%d/database-grants", op)
	code, body := serveGrants(pool, grantsFleet("orders", "billing"), "PUT",
		base+"/orders", `{"role":"operator"}`, admin)
	if code != http.StatusOK {
		t.Fatalf("grant orders: %d %v", code, body)
	}
	shrunk := grantsFleet("billing")
	code, body = serveGrants(pool, shrunk, "PUT", base+"/orders", `{"role":"viewer"}`, admin)
	if code != http.StatusBadRequest || body["code"] != "unknown_database" {
		t.Fatalf("grant on a removed database: %d %v, want 400 unknown_database", code, body)
	}
	code, body = serveGrants(pool, shrunk, "DELETE", base+"/orders", "", admin)
	if code != http.StatusOK || body["revoked"] != true {
		t.Fatalf("revoke on a removed database: %d %v, want 200", code, body)
	}
	code, body = serveGrants(pool, shrunk, "GET", base, "", admin)
	if grants, _ := body["grants"].([]any); code != http.StatusOK || len(grants) != 0 ||
		body["scoped"] != false {
		t.Fatalf("after revoke: %d %v, want no grants", code, body)
	}
	code, body = serveGrants(pool, shrunk, "DELETE", base+"/all", "", admin)
	if code != http.StatusBadRequest || body["code"] != "unknown_database" {
		t.Fatalf("revoke all: %d %v, want 400 unknown_database", code, body)
	}
}
