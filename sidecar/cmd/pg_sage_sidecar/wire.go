package main

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// WireParams captures all inputs for building the API router.
// Each field corresponds to a global variable or startup artifact
// that startAPIServer previously read directly.
type WireParams struct {
	Cfg       *config.Config
	Pool      *pgxpool.Pool // global pool (standalone or meta-db)
	FleetMgr  *fleet.DatabaseManager
	LLMMgr    *llm.Manager
	MetaState *metaDBState
	Actions   struct {
		Store    *store.ActionStore
		Executor *executor.Executor
	}
	RateLimiter *RateLimiter
}

// WireResult holds the assembled router and resolved deps for
// inspection by tests.
type WireResult struct {
	Handler    http.Handler
	AuthPool   *pgxpool.Pool
	ActionDeps *api.ActionDeps
	DBDeps     *api.DatabaseDeps
}

// wireRouter assembles the HTTP handler from the given params.
// This is the testable core extracted from startAPIServer.
func wireRouter(p WireParams) WireResult {
	// Session auth pool: prefer fleet "all" pool, fall back to
	// global pool (meta-db or standalone).
	var authPool *pgxpool.Pool
	if p.FleetMgr != nil {
		authPool = p.FleetMgr.PoolForDatabase("all")
	}
	if authPool == nil {
		authPool = p.Pool
	}

	// ActionDeps: standalone vs fleet.
	var actionDeps *api.ActionDeps
	if p.Actions.Store != nil && p.Actions.Executor != nil {
		actionDeps = &api.ActionDeps{
			Store:    p.Actions.Store,
			Executor: p.Actions.Executor,
		}
	} else if p.FleetMgr != nil {
		actionDeps = &api.ActionDeps{
			Fleet: p.FleetMgr,
		}
	}

	// DatabaseDeps: meta-db → standalone → nil.
	var dbDeps *api.DatabaseDeps
	if p.MetaState != nil && p.MetaState.Store != nil {
		metaState := p.MetaState
		dbDeps = &api.DatabaseDeps{
			Store: metaState.Store,
			Fleet: p.FleetMgr,
			OnCreate: func(rec store.DatabaseRecord) {
				registerStoreDatabase(metaState, rec)
			},
		}
	} else if p.Pool != nil {
		dbDeps = &api.DatabaseDeps{
			Store: store.NewDatabaseStore(p.Pool, nil),
		}
	}

	// Build middlewares.
	middlewares := []func(http.Handler) http.Handler{
		api.SessionAuthMiddleware(authPool),
	}
	if p.RateLimiter != nil {
		rl := p.RateLimiter
		middlewares = append(middlewares,
			func(next http.Handler) http.Handler {
				return rateLimitMiddleware(rl, next)
			})
	}

	router := api.NewRouterFull(
		p.FleetMgr, p.Cfg, authPool, actionDeps, dbDeps,
		p.LLMMgr, middlewares...,
	)

	return WireResult{
		Handler:    router,
		AuthPool:   authPool,
		ActionDeps: actionDeps,
		DBDeps:     dbDeps,
	}
}
