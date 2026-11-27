package api

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Test-only router constructors (G6-D01): production wires
// NewRouterFullRuntime; tests build routers without runtime deps.
// Production always passes a config controller (config-10); a test that
// supplies none gets one seeded from its cfg, as every startup mode does.

// NewRouter creates the API + dashboard HTTP handler.
// Pool is required for session-based auth queries.
// Middlewares wrap /api/v1/* routes (auth, rate limiting).
func NewRouter(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	middlewares ...func(http.Handler) http.Handler,
) http.Handler {
	return NewRouterWithActions(mgr, cfg, pool, nil, middlewares...)
}

// NewRouterWithActions creates the API handler with optional
// action management routes.
func NewRouterWithActions(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	actions *ActionDeps,
	middlewares ...func(http.Handler) http.Handler,
) http.Handler {
	return NewRouterFull(
		mgr, cfg, pool, actions, nil, nil, middlewares...)
}

// NewRouterFull creates the API handler with all optional deps.
func NewRouterFull(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	actions *ActionDeps,
	dbDeps *DatabaseDeps,
	llmMgr *llm.Manager,
	middlewares ...func(http.Handler) http.Handler,
) http.Handler {
	return newTestRouter(
		mgr, cfg, pool, actions, dbDeps, llmMgr, nil, middlewares...,
	)
}

// newTestRouter calls NewRouterFullRuntime with a config controller built
// from cfg when the test supplies none, and panics on a construction error.
func newTestRouter(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	actions *ActionDeps,
	dbDeps *DatabaseDeps,
	llmMgr *llm.Manager,
	runtime *RuntimeDeps,
	middlewares ...func(http.Handler) http.Handler,
) http.Handler {
	rt := RuntimeDeps{}
	if runtime != nil {
		rt = *runtime
	}
	if rt.ConfigController == nil {
		rt.ConfigController = config.NewConfigController(cfg, nil)
	}
	handler, err := NewRouterFullRuntime(
		mgr, cfg, pool, actions, dbDeps, llmMgr, &rt, middlewares...)
	if err != nil {
		panic(err)
	}
	return handler
}
