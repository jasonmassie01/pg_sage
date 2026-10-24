package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// The Change Review routes beside the managed-database routes (fleet and
// meta-db mode): "{db}/change-reviews" and "managed/{id}" overlap with
// neither more specific, which ServeMux refuses at registration with a
// panic, so the per-database routes must go through the per-database
// catch-all. The runtime always passes the registry.

func buildFleetChangeReviewRouter(t *testing.T) (h http.Handler) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("router registration panicked: %v", r)
		}
	}()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	pool := unreachablePool(t)
	dbDeps := &DatabaseDeps{Store: &store.DatabaseStore{}, Fleet: mgr}
	rt := &RuntimeDeps{DisableConfigWrites: true, ChangeReview: changereview.NewRegistry()}
	return NewRouterFullRuntime(mgr, cfg, pool, &ActionDeps{Fleet: mgr}, dbDeps, nil, rt,
		injectCharRole)
}

func TestRouter_ChangeReviewBesideManagedDatabaseRoutes(t *testing.T) {
	h := buildFleetChangeReviewRouter(t)
	want := map[routeProbe]string{
		{"POST", "/api/v1/databases/orders/change-reviews"}:          "gate:viewer",
		{"GET", "/api/v1/databases/orders/change-reviews"}:           "gate:viewer",
		{"GET", "/api/v1/databases/orders/change-reviews/7"}:         "gate:viewer",
		{"POST", "/api/v1/databases/orders/change-reviews/7/verdict"}: "gate:operator",
		{"GET", "/api/v1/change-reviews/summary"}:                     "gate:viewer",
		{"GET", "/api/v1/databases/managed/7"}:                        "gate:admin",
		{"POST", "/api/v1/databases/managed/7/test"}:                  "gate:admin",
		{"GET", "/api/v1/databases/orders/investigations"}:           "gate:viewer",
	}
	for p, class := range want {
		if got := classifyRoute(h, p); got != class {
			t.Errorf("%s %s => %s, want %s", p.method, p.path, got, class)
		}
	}
}
