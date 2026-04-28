package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// routerShutdownCtx is the context used by long-running router-owned
// goroutines (currently the OAuth CSRF-state cleaner). main.go sets
// this to the process shutdown context before building the router so
// those goroutines exit cleanly on SIGINT/SIGTERM. Tests and default
// callers leave it as context.Background(), which matches the prior
// never-cancelled behavior.
var (
	routerShutdownCtxMu sync.RWMutex
	routerShutdownCtx   = context.Background()

	// defaultBroker is the process-wide SSE broker. It is started
	// once per router build using the shutdown context, and its
	// lifetime matches the server process. Tests that build a
	// router without fleet wiring skip the broker start — the
	// handler still registers, but the event stream stays idle.
	defaultBrokerOnce sync.Once
	defaultBroker     = NewEventBroker()
)

// DefaultEventBroker returns the process-wide broker so internal
// components (tests, hand-triggered publishes) can push events.
func DefaultEventBroker() *EventBroker { return defaultBroker }

// SetShutdownContext installs a cancellable context that router-owned
// background goroutines will observe for shutdown. Call once from main
// before constructing the router.
func SetShutdownContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	routerShutdownCtxMu.Lock()
	routerShutdownCtx = ctx
	routerShutdownCtxMu.Unlock()
}

// shutdownContext returns the current router shutdown context.
func shutdownContext() context.Context {
	routerShutdownCtxMu.RLock()
	defer routerShutdownCtxMu.RUnlock()
	return routerShutdownCtx
}

// ActionDeps holds optional dependencies for action management
// routes. Pass nil to skip registering action routes.
// In fleet mode, Fleet is set so handlers can dynamically
// resolve the current pool (survives database delete/re-add).
type ActionDeps struct {
	Store    *store.ActionStore
	Executor *executor.Executor
	Fleet    *fleet.DatabaseManager
}

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
	apiMux := http.NewServeMux()
	registerAPIRoutes(apiMux, mgr, cfg, llmMgr)
	if pool != nil {
		var oauthProvider *auth.OAuthProvider
		if cfg.OAuth.Enabled {
			oauthProvider = auth.NewOAuthProvider(&cfg.OAuth)
			if err := oauthProvider.Discover(
				context.Background(),
			); err != nil {
				slog.Error("oauth discovery failed",
					"error", err)
				oauthProvider = nil
			} else {
				go oauthProvider.StartStateCleaner(
					shutdownContext())
			}
		}
		registerAuthRoutes(apiMux, pool, oauthProvider, cfg)
		registerUserRoutes(apiMux, pool)
		registerConfigRoutes(apiMux, pool, cfg, mgr)
		registerNotificationRoutes(apiMux, pool)
	}
	if actions != nil && (actions.Store != nil ||
		actions.Fleet != nil) {
		registerActionRoutes(apiMux, actions)
	}
	if dbDeps != nil && dbDeps.Store != nil {
		registerDatabaseRoutes(apiMux, dbDeps)
	}

	// Stack middlewares onto API routes only.
	var apiHandler http.Handler = apiMux
	for i := len(middlewares) - 1; i >= 0; i-- {
		apiHandler = middlewares[i](apiHandler)
	}
	// Always apply body size limit, CORS, security headers,
	// JSON content-type validation, and a per-request deadline
	// to API routes. timeoutMiddleware is applied outside the
	// body/JSON middlewares so its context also covers body
	// parsing.
	apiHandler = requireJSONMiddleware(apiHandler)
	apiHandler = maxBodyMiddleware(apiHandler)
	apiHandler = timeoutMiddleware(apiHandler)
	apiHandler = securityHeadersMiddleware(apiHandler)
	apiHandler = corsMiddleware(apiHandler)

	// Top-level mux: API routes get auth, static does not.
	root := http.NewServeMux()
	root.Handle("/api/v1/", apiHandler)

	// Embedded dashboard (SPA fallback).
	staticSub, _ := fs.Sub(staticFiles, "dist")
	fileServer := http.FileServer(http.FS(staticSub))
	root.HandleFunc("/", func(
		w http.ResponseWriter, r *http.Request,
	) {
		path := r.URL.Path
		if path == "/" || !strings.Contains(path, ".") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})

	return root
}

func registerAPIRoutes(
	mux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	llmMgr *llm.Manager,
) {
	adminOnly := RequireRole("admin")
	operatorUp := RequireRole("admin", "operator")

	mux.HandleFunc(
		"GET /api/v1/databases", databasesHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/findings", findingsListHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/findings/{id}",
		findingDetailHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/cases", casesHandler(mgr))
	mux.Handle("GET /api/v1/shadow-report",
		operatorUp(http.HandlerFunc(shadowReportHandler(mgr))))

	suppressH := operatorUp(http.HandlerFunc(
		suppressHandler(mgr)))
	mux.Handle(
		"POST /api/v1/findings/{id}/suppress", suppressH)

	unsuppressH := operatorUp(http.HandlerFunc(
		unsuppressHandler(mgr)))
	mux.Handle(
		"POST /api/v1/findings/{id}/unsuppress",
		unsuppressH)

	mux.HandleFunc(
		"GET /api/v1/actions", actionsListHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/actions/{id}",
		actionDetailHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/forecasts", forecastsHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/query-hints", queryHintsHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/alert-log", alertLogHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/snapshots/latest",
		snapshotLatestHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/snapshots/history",
		snapshotHistoryHandler(mgr))
	// v0.12 — Fleet-wide health time-series (ui-redesign-v2 §5 Overview).
	mux.HandleFunc(
		"GET /api/v1/fleet/health",
		fleetHealthHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/fleet/readiness",
		fleetReadinessHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/config", configGetHandler(mgr, cfg))

	configPutH := adminOnly(http.HandlerFunc(
		configUpdateHandler(mgr, cfg)))
	mux.Handle("PUT /api/v1/config", configPutH)

	mux.HandleFunc(
		"GET /api/v1/metrics", metricsHandler(mgr))

	stopH := operatorUp(http.HandlerFunc(
		emergencyStopHandler(mgr)))
	mux.Handle("POST /api/v1/emergency-stop", stopH)

	resumeH := operatorUp(http.HandlerFunc(
		resumeHandler(mgr)))
	mux.Handle("POST /api/v1/resume", resumeH)

	mux.HandleFunc(
		"GET /api/v1/llm/models",
		listModelsHandler(&cfg.LLM))
	mux.HandleFunc(
		"POST /api/v1/llm/models",
		discoverModelsHandler(&cfg.LLM))
	mux.HandleFunc(
		"GET /api/v1/llm/status",
		llmStatusHandler(llmMgr))

	budgetResetH := adminOnly(http.HandlerFunc(
		llmBudgetResetHandler(llmMgr)))
	mux.Handle(
		"POST /api/v1/llm/budget/reset", budgetResetH)

	// v0.9 — Incident endpoints
	mux.HandleFunc(
		"GET /api/v1/incidents",
		incidentsListHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/incidents/active",
		incidentsActiveHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/incidents/{id}",
		incidentDetailHandler(mgr))
	resolveH := operatorUp(http.HandlerFunc(
		incidentResolveHandler(mgr)))
	mux.Handle(
		"POST /api/v1/incidents/{id}/resolve", resolveH)

	// v0.9 — Explain endpoint
	var explainLLM *llm.Client
	if llmMgr != nil {
		explainLLM = llmMgr.General
	}
	explainH := operatorUp(http.HandlerFunc(
		explainHandler(mgr, cfg, explainLLM)))
	mux.Handle("POST /api/v1/explain", explainH)

	// v0.9 — Growth forecast endpoint
	mux.HandleFunc(
		"GET /api/v1/forecasts/growth",
		growthForecastHandler(mgr))

	// v0.11 — Findings stats aggregate (replaces the old
	// /api/v1/schema/findings + /stats split). Schema-lint rows
	// live in sage.findings under category LIKE 'schema_lint:%';
	// callers pass source=schema_lint to filter to that subsystem.
	mux.HandleFunc(
		"GET /api/v1/findings/stats",
		findingsStatsHandler(mgr))

	// SSE live-update stream. Broker starts once per process.
	if mgr != nil {
		defaultBrokerOnce.Do(func() {
			defaultBroker.Start(
				shutdownContext(), mgr,
				2*time.Second, 15*time.Second,
			)
		})
	}
	mux.HandleFunc(
		"GET /api/v1/events", eventsHandler(defaultBroker))
}

func registerAuthRoutes(
	mux *http.ServeMux,
	pool *pgxpool.Pool,
	oauthProvider *auth.OAuthProvider,
	cfg *config.Config,
) {
	mux.HandleFunc(
		"POST /api/v1/auth/login", loginHandler(pool))
	mux.HandleFunc(
		"POST /api/v1/auth/logout", logoutHandler(pool))
	mux.HandleFunc(
		"GET /api/v1/auth/me", meHandler())

	// OAuth routes (always registered; return disabled if not configured).
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/config",
		oauthConfigHandler(oauthProvider, cfg.OAuth.Provider))
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/authorize",
		oauthAuthorizeHandler(oauthProvider))
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/callback",
		oauthCallbackHandler(
			oauthProvider, pool,
			cfg.OAuth.DefaultRole, cfg.OAuth.Provider))
}

func registerUserRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool,
) {
	adminOnly := RequireRole("admin")

	listH := adminOnly(http.HandlerFunc(
		listUsersHandler(pool)))
	mux.Handle("GET /api/v1/users", listH)

	createH := adminOnly(http.HandlerFunc(
		createUserHandler(pool)))
	mux.Handle("POST /api/v1/users", createH)

	deleteH := adminOnly(http.HandlerFunc(
		deleteUserHandler(pool)))
	mux.Handle("DELETE /api/v1/users/{id}", deleteH)

	roleH := adminOnly(http.HandlerFunc(
		updateUserRoleHandler(pool)))
	mux.Handle("PUT /api/v1/users/{id}/role", roleH)
}

func registerConfigRoutes(
	mux *http.ServeMux,
	pool *pgxpool.Pool,
	cfg *config.Config,
	mgr ...*fleet.DatabaseManager,
) {
	adminOnly := RequireRole("admin")
	cs := store.NewConfigStore(pool)
	baseCfg := config.Clone(cfg)

	var fm *fleet.DatabaseManager
	if len(mgr) > 0 {
		fm = mgr[0]
	}

	globalGet := adminOnly(http.HandlerFunc(
		configGlobalGetHandler(cs, baseCfg)))
	mux.Handle("GET /api/v1/config/global", globalGet)

	globalPut := adminOnly(http.HandlerFunc(
		configGlobalPutHandler(cs, cfg, fm)))
	mux.Handle("PUT /api/v1/config/global", globalPut)

	globalDelete := adminOnly(http.HandlerFunc(
		configGlobalDeleteHandler(cs, cfg, baseCfg, fm)))
	mux.Handle("DELETE /api/v1/config/global/{key}", globalDelete)

	dbGet := adminOnly(http.HandlerFunc(
		configDBGetHandler(cs, baseCfg, pool)))
	mux.Handle(
		"GET /api/v1/config/databases/{id}", dbGet)

	dbPut := adminOnly(http.HandlerFunc(
		configDBPutHandler(cs, cfg, pool, fm)))
	mux.Handle(
		"PUT /api/v1/config/databases/{id}", dbPut)

	dbDelete := adminOnly(http.HandlerFunc(
		configDBDeleteHandler(cs, cfg, fm)))
	mux.Handle(
		"DELETE /api/v1/config/databases/{id}/{key}",
		dbDelete)

	audit := adminOnly(http.HandlerFunc(
		configAuditHandler(cs)))
	mux.Handle("GET /api/v1/config/audit", audit)
}

func registerActionRoutes(
	mux *http.ServeMux,
	deps *ActionDeps,
) {
	operatorUp := RequireRole("admin", "operator")

	if deps.Fleet != nil {
		// Fleet mode: dynamically resolve pool on each
		// request so delete/re-add cycles don't break.
		pendingH := operatorUp(http.HandlerFunc(
			fleetPendingActionsHandler(deps.Fleet)))
		mux.Handle(
			"GET /api/v1/actions/pending", pendingH)
		countH := operatorUp(http.HandlerFunc(
			fleetPendingCountHandler(deps.Fleet)))
		mux.Handle(
			"GET /api/v1/actions/pending/count", countH)
	} else {
		pendingH := operatorUp(http.HandlerFunc(
			pendingActionsHandler(deps.Store, deps.Executor)))
		mux.Handle(
			"GET /api/v1/actions/pending", pendingH)
		countH := operatorUp(http.HandlerFunc(
			pendingCountHandler(deps.Store)))
		mux.Handle(
			"GET /api/v1/actions/pending/count", countH)
	}

	// Inline action flow on the Findings page — per-finding
	// lookup of any pending queued actions. Works in both
	// fleet and standalone modes.
	byFindingH := operatorUp(http.HandlerFunc(
		findingPendingActionsHandler(deps)))
	mux.Handle(
		"GET /api/v1/findings/{id}/pending-actions",
		byFindingH)

	if deps.Store != nil && deps.Executor != nil {
		approveH := operatorUp(http.HandlerFunc(
			approveActionHandler(
				deps.Store, deps.Executor)))
		mux.Handle(
			"POST /api/v1/actions/{id}/approve", approveH)

		rejectH := operatorUp(http.HandlerFunc(
			rejectActionHandler(deps.Store)))
		mux.Handle(
			"POST /api/v1/actions/{id}/reject", rejectH)

		rollbackH := operatorUp(http.HandlerFunc(
			rollbackActionHandler(deps.Executor)))
		mux.Handle(
			"POST /api/v1/actions/{id}/rollback", rollbackH)

		execH := operatorUp(http.HandlerFunc(
			manualExecuteHandler(deps.Executor)))
		mux.Handle(
			"POST /api/v1/actions/execute", execH)
	} else if deps.Fleet != nil {
		approveH := operatorUp(http.HandlerFunc(
			fleetApproveActionHandler(deps.Fleet)))
		mux.Handle(
			"POST /api/v1/actions/{id}/approve", approveH)

		rejectH := operatorUp(http.HandlerFunc(
			fleetRejectActionHandler(deps.Fleet)))
		mux.Handle(
			"POST /api/v1/actions/{id}/reject", rejectH)

		rollbackH := operatorUp(http.HandlerFunc(
			fleetRollbackActionHandler(deps.Fleet)))
		mux.Handle(
			"POST /api/v1/actions/{id}/rollback", rollbackH)

		execH := operatorUp(http.HandlerFunc(
			fleetManualExecuteHandler(deps.Fleet)))
		mux.Handle(
			"POST /api/v1/actions/execute", execH)
	} else {
		notImpl := operatorUp(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				jsonError(w,
					"action approval not available",
					http.StatusNotImplemented)
			}))
		mux.Handle(
			"POST /api/v1/actions/{id}/approve", notImpl)
		mux.Handle(
			"POST /api/v1/actions/{id}/reject", notImpl)
		mux.Handle(
			"POST /api/v1/actions/{id}/rollback", notImpl)
		mux.Handle(
			"POST /api/v1/actions/execute", notImpl)
	}
}

func registerNotificationRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool,
) {
	adminOnly := RequireRole("admin")
	d := newDefaultDispatcher(pool)
	ns := store.NewNotificationStore(pool, d)

	chList := adminOnly(http.HandlerFunc(
		listChannelsHandler(ns)))
	mux.Handle(
		"GET /api/v1/notifications/channels", chList)

	chCreate := adminOnly(http.HandlerFunc(
		createChannelHandler(ns)))
	mux.Handle(
		"POST /api/v1/notifications/channels", chCreate)

	chUpdate := adminOnly(http.HandlerFunc(
		updateChannelHandler(ns)))
	mux.Handle(
		"PUT /api/v1/notifications/channels/{id}",
		chUpdate)

	chDelete := adminOnly(http.HandlerFunc(
		deleteChannelHandler(ns)))
	mux.Handle(
		"DELETE /api/v1/notifications/channels/{id}",
		chDelete)

	chTest := adminOnly(http.HandlerFunc(
		testChannelHandler(ns)))
	mux.Handle(
		"POST /api/v1/notifications/channels/{id}/test",
		chTest)

	ruleList := adminOnly(http.HandlerFunc(
		listRulesHandler(ns)))
	mux.Handle(
		"GET /api/v1/notifications/rules", ruleList)

	ruleCreate := adminOnly(http.HandlerFunc(
		createRuleHandler(ns)))
	mux.Handle(
		"POST /api/v1/notifications/rules", ruleCreate)

	ruleDelete := adminOnly(http.HandlerFunc(
		deleteRuleHandler(ns)))
	mux.Handle(
		"DELETE /api/v1/notifications/rules/{id}",
		ruleDelete)

	ruleUpdate := adminOnly(http.HandlerFunc(
		updateRuleHandler(ns)))
	mux.Handle(
		"PUT /api/v1/notifications/rules/{id}",
		ruleUpdate)

	logList := adminOnly(http.HandlerFunc(
		listNotificationLogHandler(ns)))
	mux.Handle(
		"GET /api/v1/notifications/log", logList)
}

func newDefaultDispatcher(
	pool *pgxpool.Pool,
) *notify.Dispatcher {
	logFn := func(_, _ string, _ ...any) {}
	d := notify.NewDispatcher(pool, logFn)
	d.RegisterSender(notify.NewSlackSender())
	d.RegisterSender(notify.NewEmailSender())
	d.RegisterSender(notify.NewPagerDutySender())
	return d
}
