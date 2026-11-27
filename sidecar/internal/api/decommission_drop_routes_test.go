package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/decommission"
)

// The legacy table drop is irreversible, so it is admin-only like the
// inventory, and absent without a control pool.
func TestDecommissionDropRoute_AdminOnly(t *testing.T) {
	h := buildStandaloneRouter(t, injectCharRole)
	p := routeProbe{http.MethodPost, decommission.DropPath}
	for role, want := range map[string]int{"operator": http.StatusForbidden,
		"viewer": http.StatusForbidden, "": http.StatusUnauthorized} {
		if code, body := serveProbe(h, p, role); code != want {
			t.Errorf("POST %s as %q = %d %s, want %d", p.path, role, code, body, want)
		}
	}
	code, body := serveProbe(h, p, "admin")
	if code != http.StatusBadRequest || !strings.Contains(body, decommission.DropConfirmPhrase) {
		t.Errorf("admin POST %s {} = %d %s, want the handler's 400 for the phrase",
			p.path, code, body)
	}
	bare := buildBareRouter(t, injectCharRole)
	if code, _ := serveProbe(bare, p, "admin"); code != http.StatusNotFound {
		t.Errorf("without a control pool the drop route = %d, want 404", code)
	}
}
