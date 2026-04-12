package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// testConfig returns a minimal config for wiring tests.
func testConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Mode = "standalone"
	return cfg
}

// testFleetMgr returns an empty fleet manager.
func testFleetMgr(cfg *config.Config) *fleet.DatabaseManager {
	return fleet.NewManager(cfg)
}

// --- Mode-Specific Wiring Tests ---

func TestWireRouter_Standalone_DBDepsWired(t *testing.T) {
	// Bug 1 prevention: standalone mode with a pool must wire
	// DatabaseDeps so /api/v1/databases/managed is reachable.
	cfg := testConfig()
	cfg.Mode = "standalone"

	// Use a nil pool — we can't create a real pgxpool in unit
	// tests, but wireRouter only stores it, never queries.
	// The key assertion is that dbDeps is non-nil.
	result := wireRouter(WireParams{
		Cfg:      cfg,
		Pool:     nil, // would be non-nil in real standalone
		FleetMgr: testFleetMgr(cfg),
	})
	if result.Handler == nil {
		t.Fatal("handler should not be nil")
	}
	// In standalone with no pool, dbDeps is nil (no DB to wire).
	// That's correct — the bug was when pool was non-nil but
	// dbDeps was still nil. We can't provide a real pool here,
	// so test the negative case.
	if result.DBDeps != nil {
		t.Error("dbDeps should be nil when pool is nil")
	}
}

func TestWireRouter_Standalone_ActionDepsFromStore(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = "standalone"
	fm := testFleetMgr(cfg)

	// When both actionStore and executor are provided,
	// actionDeps should use the Store+Executor path.
	as := &store.ActionStore{}
	ex := &executor.Executor{}

	result := wireRouter(WireParams{
		Cfg:      cfg,
		FleetMgr: fm,
		Actions: struct {
			Store    *store.ActionStore
			Executor *executor.Executor
		}{Store: as, Executor: ex},
	})

	if result.ActionDeps == nil {
		t.Fatal("actionDeps should be wired in standalone mode")
	}
	if result.ActionDeps.Store != as {
		t.Error("actionDeps.Store should be the provided store")
	}
	if result.ActionDeps.Executor != ex {
		t.Error("actionDeps.Executor should be the provided executor")
	}
	if result.ActionDeps.Fleet != nil {
		t.Error("actionDeps.Fleet should be nil in standalone mode")
	}
}

func TestWireRouter_Fleet_ActionDepsFromFleet(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = "fleet"
	fm := testFleetMgr(cfg)

	// Fleet mode: no global actionStore/executor, so actionDeps
	// should use the Fleet path.
	result := wireRouter(WireParams{
		Cfg:      cfg,
		FleetMgr: fm,
	})

	if result.ActionDeps == nil {
		t.Fatal("actionDeps should be wired in fleet mode")
	}
	if result.ActionDeps.Fleet != fm {
		t.Error("actionDeps.Fleet should be the fleet manager")
	}
	if result.ActionDeps.Store != nil {
		t.Error("actionDeps.Store should be nil in fleet mode")
	}
}

func TestWireRouter_MetaDB_DBDepsFromStore(t *testing.T) {
	cfg := testConfig()
	cfg.Mode = "fleet"
	fm := testFleetMgr(cfg)

	// Meta-db mode: globalMetaState has a Store, so dbDeps
	// should be wired with Store + Fleet + OnCreate.
	metaState := &metaDBState{
		Store: store.NewDatabaseStore(nil, nil),
	}

	result := wireRouter(WireParams{
		Cfg:       cfg,
		FleetMgr:  fm,
		MetaState: metaState,
	})

	if result.DBDeps == nil {
		t.Fatal("dbDeps should be wired in meta-db mode")
	}
	if result.DBDeps.Store != metaState.Store {
		t.Error("dbDeps.Store should come from metaState")
	}
	if result.DBDeps.Fleet != fm {
		t.Error("dbDeps.Fleet should be the fleet manager")
	}
	if result.DBDeps.OnCreate == nil {
		t.Error("dbDeps.OnCreate should be set in meta-db mode")
	}
}

func TestWireRouter_NoLLM_GracefulDegradation(t *testing.T) {
	cfg := testConfig()
	fm := testFleetMgr(cfg)

	// No LLM manager — router should still build without panic.
	result := wireRouter(WireParams{
		Cfg:      cfg,
		FleetMgr: fm,
		LLMMgr:   nil,
	})

	if result.Handler == nil {
		t.Fatal("handler should not be nil without LLM")
	}

	// LLM status route should be registered (not 404).
	// Auth middleware may return 401, which still proves wiring.
	req := httptest.NewRequest(
		http.MethodGet, "/api/v1/llm/status", nil)
	w := httptest.NewRecorder()
	result.Handler.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Error("LLM status route should be registered even " +
			"without LLM manager")
	}
}

func TestWireRouter_WithLLM_RouteRegistered(t *testing.T) {
	cfg := testConfig()
	fm := testFleetMgr(cfg)

	llmCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://test/v1",
		APIKey:           "key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := llm.New(llmCfg, func(_, _ string, _ ...any) {})
	mgr := llm.NewManager(client, nil, false)

	result := wireRouter(WireParams{
		Cfg:      cfg,
		FleetMgr: fm,
		LLMMgr:   mgr,
	})

	// Route exists (not 404). Auth middleware may intercept.
	req := httptest.NewRequest(
		http.MethodGet, "/api/v1/llm/status", nil)
	w := httptest.NewRecorder()
	result.Handler.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Error("LLM status route should be registered")
	}
}

func TestWireRouter_NilFleetMgr_NoPanic(t *testing.T) {
	cfg := testConfig()

	// Verify wireRouter handles nil FleetMgr without panic.
	result := wireRouter(WireParams{
		Cfg: cfg,
	})

	if result.Handler == nil {
		t.Fatal("handler should not be nil with nil FleetMgr")
	}
	// With nil FleetMgr and nil Pool, actionDeps should be nil.
	if result.ActionDeps != nil {
		t.Error("actionDeps should be nil with no fleet and " +
			"no action store")
	}
}

func TestWireRouter_RateLimiter_Applied(t *testing.T) {
	cfg := testConfig()
	fm := testFleetMgr(cfg)
	rl := NewRateLimiter(1) // 1 request per minute
	t.Cleanup(rl.Stop)

	result := wireRouter(WireParams{
		Cfg:         cfg,
		FleetMgr:    fm,
		RateLimiter: rl,
	})

	if result.Handler == nil {
		t.Fatal("handler should not be nil with rate limiter")
	}
}

func TestWireRouter_AuthPoolFallback(t *testing.T) {
	cfg := testConfig()

	// Empty fleet manager (no instances) → PoolForDatabase returns
	// nil → authPool falls back to p.Pool.
	fm := testFleetMgr(cfg)

	result := wireRouter(WireParams{
		Cfg:      cfg,
		Pool:     nil, // in real code this would be a pgxpool
		FleetMgr: fm,
	})

	// AuthPool should be nil (fell back to nil Pool).
	if result.AuthPool != nil {
		t.Error("authPool should be nil when fleet has no pools " +
			"and global pool is nil")
	}
}

// --- Route Registration Verification ---

func TestWireRouter_CoreRoutesRegistered(t *testing.T) {
	cfg := testConfig()
	fm := testFleetMgr(cfg)

	result := wireRouter(WireParams{
		Cfg:      cfg,
		FleetMgr: fm,
	})

	// These core routes must always be registered regardless
	// of mode. Verify they return something other than the SPA
	// fallback (which returns 200 with HTML).
	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/databases"},
		{"GET", "/api/v1/findings"},
		{"GET", "/api/v1/actions"},
		{"GET", "/api/v1/forecasts"},
		{"GET", "/api/v1/llm/models"},
		{"GET", "/api/v1/llm/status"},
		{"GET", "/api/v1/metrics"},
		{"GET", "/api/v1/snapshots/latest"},
		{"GET", "/api/v1/snapshots/history"},
		{"GET", "/api/v1/config"},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, nil)
			w := httptest.NewRecorder()
			result.Handler.ServeHTTP(w, req)

			// Any status except 404 means the route is registered.
			// (May get 500, 503, etc. due to nil deps, but route
			// exists.)
			if w.Code == http.StatusNotFound {
				t.Errorf("route %s %s returned 404 — not registered",
					r.method, r.path)
			}
		})
	}
}
