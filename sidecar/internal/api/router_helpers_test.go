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
	return NewRouterFullRuntime(
		mgr, cfg, pool, actions, dbDeps, llmMgr, nil, middlewares...,
	)
}
