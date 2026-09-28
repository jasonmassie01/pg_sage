package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// The per-database investigation routes share /api/v1/databases/ with the
// managed-database routes registered in fleet mode (a database store is
// configured). Both sets must register on one mux, and each request must
// reach its own handler: the full sidecar router panicked at startup when
// "GET /api/v1/databases/{db}/investigations" met
// "GET /api/v1/databases/managed/{id}".
func TestSREAPI_CoexistsWithManagedDatabaseRoutes(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	var h http.Handler
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("router registration panicked: %v", p)
			}
		}()
		h = managedAndSRERouter(mgr)
	}()
	code, body, _ := sreCall(t, h, "GET", "/api/v1/databases/orders/investigations/"+
		string(orders.ID))
	if code != http.StatusOK {
		t.Fatalf("investigation detail = %d %s", code, body)
	}
	// A viewer reaching the admin-only managed route is refused by that
	// route, not answered by an investigation handler.
	code, body, _ = sreCall(t, h, "GET", "/api/v1/databases/managed/7")
	if code != http.StatusForbidden {
		t.Fatalf("managed database route = %d %s, want 403 from the admin gate", code, body)
	}
	code, _, _ = sreCall(t, h, "GET", "/api/v1/databases/orders/unknown")
	if code != http.StatusNotFound {
		t.Fatalf("unknown database sub-route = %d, want 404", code)
	}
}

func managedAndSRERouter(mgr *fleet.DatabaseManager) http.Handler {
	user := testViewerUser()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	deps := &DatabaseDeps{Store: &store.DatabaseStore{}, Fleet: mgr}
	return NewRouterFullRuntime(mgr, config.DefaultConfig(), nil, nil, deps, nil, nil,
		inject)
}
