package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

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
					context.Background())
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
	// and JSON content-type validation to API routes.
	apiHandler = requireJSONMiddleware(apiHandler)
	apiHandler = maxBodyMiddleware(apiHandler)
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
		"GET /api/v1/llm/status",
		llmStatusHandler(llmMgr))
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

	var fm *fleet.DatabaseManager
	if len(mgr) > 0 {
		fm = mgr[0]
	}

	globalGet := adminOnly(http.HandlerFunc(
		configGlobalGetHandler(cs, cfg)))
	mux.Handle("GET /api/v1/config/global", globalGet)

	globalPut := adminOnly(http.HandlerFunc(
		configGlobalPutHandler(cs, cfg, fm)))
	mux.Handle("PUT /api/v1/config/global", globalPut)

	dbGet := adminOnly(http.HandlerFunc(
		configDBGetHandler(cs, cfg, pool)))
	mux.Handle(
		"GET /api/v1/config/databases/{id}", dbGet)

	dbPut := adminOnly(http.HandlerFunc(
		configDBPutHandler(cs, cfg, pool, fm)))
	mux.Handle(
		"PUT /api/v1/config/databases/{id}", dbPut)

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
			pendingActionsHandler(deps.Store)))
		mux.Handle(
			"GET /api/v1/actions/pending", pendingH)
		countH := operatorUp(http.HandlerFunc(
			pendingCountHandler(deps.Store)))
		mux.Handle(
			"GET /api/v1/actions/pending/count", countH)
	}

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

		execH := operatorUp(http.HandlerFunc(
			manualExecuteHandler(deps.Executor)))
		mux.Handle(
			"POST /api/v1/actions/execute", execH)
	} else {
		// Fleet mode without global store/executor:
		// approve/reject/execute not yet supported
		// fleet-wide. Return 501 instead of crashing.
		notImpl := operatorUp(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				jsonError(w,
					"action approval not available "+
						"in fleet mode yet",
					http.StatusNotImplemented)
			}))
		mux.Handle(
			"POST /api/v1/actions/{id}/approve", notImpl)
		mux.Handle(
			"POST /api/v1/actions/{id}/reject", notImpl)
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
