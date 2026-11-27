package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// guard-01a x apiauth-08b/08c: with api.database_grants on, the pressure
// route reads only databases the caller may read now. A scoped viewer's
// session and a person's token whose owner lacks a grant both get the
// unknown-database 404 without a read; a grant-store failure is 503. No
// concurrent access test here: the grant check is per request and holds
// no state (the checker's concurrency is covered in internal/pressure).

// pressureGrantsMux serves orders and billing behind src (user 2 owns
// token t1, every database).
func pressureGrantsMux(t *testing.T, src DatabaseGrantSource) (http.Handler,
	*pressureSource) {
	t.Helper()
	ps := &pressureSource{known: map[string]bool{"t": true}}
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1")})
	return withDatabaseAuthz(&databaseAuthz{grants: src},
		pressureMux(pressureFleet(t, ps), tokens)), ps
}

func pressureReads(ps *pressureSource) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.tables)
}

func TestPressureRoute_OwnerGrantsLimitAPersonsToken(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(2, map[string]string{"billing": auth.RoleViewer})
	h, ps := pressureGrantsMux(t, src)
	bearer := "Bearer " + lintSecret(1)
	if rec := getPressure(t, h, "/api/v1/databases/billing/pressure?table=t", bearer,
		nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(),
		`"database":"billing"`) {
		t.Fatalf("token on a granted database: %d %s, want 200 for billing", rec.Code,
			rec.Body)
	}
	before := pressureReads(ps)
	hidden := getPressure(t, h, "/api/v1/databases/orders/pressure?table=t", bearer, nil)
	unknown := getPressure(t, h, "/api/v1/databases/nosuch/pressure?table=t", bearer, nil)
	if hidden.Code != http.StatusNotFound || hidden.Body.String() != unknown.Body.String() {
		t.Fatalf("ungranted database: %d %s, want the unknown database's 404 (%d %s)",
			hidden.Code, hidden.Body, unknown.Code, unknown.Body)
	}
	if n := pressureReads(ps) - before; n != 0 {
		t.Fatalf("%d read sets for refused databases, want 0", n)
	}
}

func TestPressureRoute_ScopedViewerSessionSeesGrantedOnly(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	h, ps := pressureGrantsMux(t, src)
	if rec := getPressure(t, h, "/api/v1/databases/billing/pressure", "",
		dbViewer(7)); rec.Code != http.StatusOK {
		t.Fatalf("scoped viewer on billing: %d %s, want 200", rec.Code, rec.Body)
	}
	before := pressureReads(ps)
	hidden := getPressure(t, h, "/api/v1/databases/orders/pressure", "", dbViewer(7))
	if hidden.Code != http.StatusNotFound ||
		!strings.Contains(hidden.Body.String(), "database not found") {
		t.Fatalf("scoped viewer on orders: %d %s, want the unknown database's 404",
			hidden.Code, hidden.Body)
	}
	if n := pressureReads(ps) - before; n != 0 {
		t.Fatalf("%d read sets for a hidden database, want 0", n)
	}
}

func TestPressureRoute_GrantStoreFailureRefusesTheToken(t *testing.T) {
	h, ps := pressureGrantsMux(t, &dbAuthzSource{err: errors.New("connection refused")})
	rec := getPressure(t, h, "/api/v1/databases/orders/pressure", "Bearer "+lintSecret(1),
		nil)
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "grant_store_unavailable") {
		t.Fatalf("grant store failure: %d %s, want 503 grant_store_unavailable", rec.Code,
			rec.Body)
	}
	if n := pressureReads(ps); n != 0 {
		t.Fatalf("%d read sets after a failed grant check, want 0", n)
	}
}

func TestPressureRoute_BearerBesideASessionIsTheOnlyCredential(t *testing.T) {
	h, ps := pressureGrantsMux(t, &dbAuthzSource{})
	admin := &auth.User{ID: 1, Email: "admin@example.test", Role: auth.RoleAdmin}
	rec := getPressure(t, h, "/api/v1/databases/orders/pressure", "Bearer "+lintSecret(8),
		admin)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token beside an admin session: %d %s, want 401 (never retried as "+
			"the session)", rec.Code, rec.Body)
	}
	if n := pressureReads(ps); n != 0 {
		t.Fatalf("%d read sets for a refused token, want 0", n)
	}
}
