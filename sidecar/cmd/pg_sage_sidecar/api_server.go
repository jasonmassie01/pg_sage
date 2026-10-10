package main

import (
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func initFleetAndAPI() {
	if fleetMgr == nil {
		fleetMgr = fleet.NewManager(cfg)
	}

	// Every mode's init registered its database runtimes.
	startMCPRuntime()

	startAPIServer(rateLimiterInstance)

	// Start session cleaner against the same canonical control pool that
	// owns authentication. Monitored pools are replaceable in meta mode.
	sessionPool := sessionControlPool(globalMetaState, fleetMgr, pool)
	if sessionPool != nil {
		go auth.StartSessionCleaner(
			shutdownCtx, sessionPool, time.Hour,
		)
	}
}

func startAPIServer(rl *RateLimiter) {
	if rl == nil {
		logError("api", "rate limiter unavailable; refusing to start API server")
		return
	}
	addr := cfg.API.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	configureAPIProcessHooks()
	result := wireRouter(WireParams{
		Cfg:       cfg,
		Pool:      pool,
		FleetMgr:  fleetMgr,
		LLMMgr:    llmMgr,
		MetaState: globalMetaState,
		Actions: struct {
			Store    *store.ActionStore
			Executor *executor.Executor
		}{
			Store:    actionStore,
			Executor: exec,
		},
		RateLimiter:      rl,
		Config:           configController,
		ConfigBase:       configBase,
		ConfigBaseLoader: loadFileConfigBase,
		LLMBudgets:       llmBudgetRegistry(),
		MCPHandler:       mcpHTTPHandler(),
	})
	startAuthPoolServices(result.AuthPool)
	serveAPI(addr, result.Handler)
}

func sessionControlPool(
	metaState *metaDBState,
	mgr *fleet.DatabaseManager,
	fallback *pgxpool.Pool,
) *pgxpool.Pool {
	if metaState != nil && metaState.Pool != nil {
		return metaState.Pool
	}
	if mgr != nil {
		if fleetPool := mgr.PoolForDatabase("all"); fleetPool != nil {
			return fleetPool
		}
	}
	return fallback
}

// configureAPIProcessHooks installs the process shutdown context and, under
// a declared supervisor, the restart hook the API router uses.
func configureAPIProcessHooks() {
	// Wire the process shutdown context into router-owned
	// goroutines (OAuth CSRF state cleaner) so they exit on
	// SIGINT/SIGTERM instead of leaking.
	api.SetShutdownContext(shutdownCtx)
	// Offer restart only under a declared supervisor; otherwise the API
	// answers 501 instead of exiting 42 into nothing (G5-B09).
	if supervisorDeclared(os.Getenv) {
		api.SetRestartFunc(triggerRestart)
	} else {
		logInfo("api", "restart endpoint disabled: set SAGE_SUPERVISED=1 "+
			"when a supervisor relaunches the sidecar on exit %d", restartExitCode)
	}
}

// startAuthPoolServices reports a missing auth pool and otherwise starts
// the control-pool services on it.
func startAuthPoolServices(authPool *pgxpool.Pool) {
	// Fail loudly (not silently) when there is no usable auth pool.
	// Without it, registerAuthRoutes is skipped and every /api/* path
	// 401s with no /auth/login to recover — a bricked dashboard that
	// otherwise looks healthy in the logs (LIVE-01/04).
	if authPool == nil {
		logError("api",
			"AUTH DISABLED: no connected database available for session "+
				"storage — dashboard login and all authenticated API "+
				"endpoints are unavailable. Ensure at least one configured "+
				"database is reachable (or configure a meta-database), then "+
				"restart.")
	}

	if authPool != nil {
		// Election first: the leader-only loops below read its result.
		startFleetLearning(shutdownCtx, authPool, fleetMgr)
		startAuditJobs(shutdownCtx, authPool, fleetMgr)
		startDecommissionReport(shutdownCtx, authPool, cfg.ConfigPath)
		startApprovalCardLoop(shutdownCtx, authPool, fleetMgr)
		startSpecialistOutbound(shutdownCtx)
	}
}

// serveAPI builds the API HTTP server for handler and starts listening on
// addr in the background.
func serveAPI(addr string, handler http.Handler) {
	apiServer = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logInfo("api", "%s", apiListenLog(addr, apiTLS))
		if err := listenAPI(apiServer, apiTLS); err != nil &&
			err != http.ErrServerClosed {
			logError("api", "server error: %v", err)
		}
	}()
}
