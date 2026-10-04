package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Roadmap 1.2: an approval card carries the trust of its action class on
// its database (level, evidence counts, path to the next level) from the
// same ledger view as the Trust page.

func (fx *cardFixture) ledgerRouter(t *testing.T, user *auth.User) http.Handler {
	t.Helper()
	st, err := earned.NewPostgresStore(fx.pool, apiUUID(t), "orders")
	if err != nil {
		t.Fatal(err)
	}
	cfg := earned.DefaultConfig()
	cfg.EvidenceCacheTTL = 0
	ledger, err := earned.NewService(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := earned.NewRegistry(true)
	reg.Register("orders", earned.RegistryEntry{Service: ledger,
		Limiter: ledger.Limiter(earned.Binding{Database: "orders"})})
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(fx.mgr, config.DefaultConfig(), nil,
		&ActionDeps{Fleet: fx.mgr}, nil, nil,
		&RuntimeDeps{Autonomy: &AutonomyDeps{Ledgers: reg}}, inject)
}

func TestApprovalCardCarriesTheClassTrust(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.ledgerRouter(t, operatorUser())
	code, one := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	card, _ := one["card"].(map[string]any)
	tr, _ := card["trust"].(map[string]any)
	if code != http.StatusOK || tr == nil {
		t.Fatalf("get: %d %v", code, one)
	}
	line, _ := tr["line"].(string)
	if tr["family"] != "hygiene" || tr["class"] != "analyze" || tr["level"] != "L1" ||
		tr["next_level"] != "L2" || !strings.Contains(line, "hygiene/analyze") {
		t.Fatalf("trust = %v", tr)
	}
	ev, _ := tr["evidence"].(map[string]any)
	for _, k := range []string{"improved", "neutral", "regressed", "rolled_back", "rejected"} {
		if _, ok := ev[k]; !ok {
			t.Fatalf("evidence lacks %q: %v", k, ev)
		}
	}
	// The list holds other tests' items of the shared database too: find
	// the fixture's own card.
	code, list := cardCall(t, h, http.MethodGet, "/api/v1/approvals?database=orders", nil)
	cards, _ := list["cards"].([]any)
	var mine map[string]any
	for _, c := range cards {
		m, _ := c.(map[string]any)
		if id, _ := m["queue_id"].(float64); int(id) == fx.queueID {
			mine = m
		}
	}
	if code != http.StatusOK || mine == nil || mine["trust"] == nil {
		t.Fatalf("list: %d, card %d = %v", code, fx.queueID, mine)
	}
}

func TestApprovalCardWithoutALedgerHasNoTrust(t *testing.T) {
	fx := newCardFixture(t)
	code, one := cardCall(t, fx.router(t, operatorUser()), http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	card, _ := one["card"].(map[string]any)
	if code != http.StatusOK || card == nil || card["trust"] != nil {
		t.Fatalf("get without ledger: %d %v", code, one)
	}
}
