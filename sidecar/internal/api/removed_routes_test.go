package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/decommission"
)

// G0-02: every route of the removed provisioning surface answers 404 to an
// admin in every router configuration, and none is exempt from session
// auth any more (an anonymous caller gets 401, never the old handler).
func TestRemovedProvisioningRoutes_Return404AndNeedAuth(t *testing.T) {
	if len(decommission.RemovedRoutes) < 50 {
		t.Fatalf("only %d removed routes listed; the manifest lists about 60",
			len(decommission.RemovedRoutes))
	}
	for _, rc := range routerConfigs() {
		h := rc.build(t)
		for _, r := range decommission.RemovedRoutes {
			p := routeProbe{method: r.Method, path: r.Path}
			if code, body := serveProbe(h, p, "admin"); code != http.StatusNotFound {
				t.Errorf("%s: admin %s %s = %d %s, want 404", rc.name, r.Method, r.Path,
					code, body)
			}
			if shouldSkipAuth(r.Path) {
				t.Errorf("%s %s is still exempt from session auth", r.Method, r.Path)
			}
		}
	}
}

func TestRemovedProvisioningRoutes_AnonymousIsUnauthorized(t *testing.T) {
	h := SessionAuthMiddleware(nil)(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	for _, r := range decommission.RemovedRoutes {
		if code, _ := serveProbe(h, routeProbe{method: r.Method, path: r.Path}, ""); code !=
			http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.Method, r.Path, code)
		}
	}
}

// The decommission inventory and acknowledgement are admin-only (§8.3).
func TestDecommissionRoutes_AdminOnly(t *testing.T) {
	h := buildStandaloneRouter(t)
	for _, p := range []routeProbe{
		{http.MethodGet, decommission.InventoryPath},
		{http.MethodPost, decommission.AckPath},
	} {
		for role, want := range map[string]int{"operator": http.StatusForbidden,
			"viewer": http.StatusForbidden, "": http.StatusUnauthorized} {
			if code, body := serveProbe(h, p, role); code != want {
				t.Errorf("%s %s as %q = %d %s, want %d", p.method, p.path, role, code,
					body, want)
			}
		}
		code, body := serveProbe(h, p, "admin")
		if code == http.StatusNotFound || code == http.StatusForbidden ||
			code == http.StatusUnauthorized || strings.Contains(body, "insufficient") {
			t.Errorf("admin %s %s = %d %s, want the handler", p.method, p.path, code, body)
		}
	}
	bare := buildBareRouter(t)
	if code, _ := serveProbe(bare, routeProbe{http.MethodGet, decommission.InventoryPath},
		"admin"); code != http.StatusNotFound {
		t.Errorf("without a control pool the inventory route = %d, want 404", code)
	}
}
