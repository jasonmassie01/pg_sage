package api

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// ActionDeps holds optional dependencies for action management
// routes. Pass nil to skip registering action routes.
type ActionDeps struct {
	Store    *store.ActionStore
	Executor *executor.Executor
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
		mgr, cfg, pool, actions, nil, middlewares...)
}

// NewRouterFull creates the API handler with all optional deps.
func NewRouterFull(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	actions *ActionDeps,
	dbDeps *DatabaseDeps,
	middlewares ...func(http.Handler) http.Handler,
) http.Handler {
	apiMux := http.NewServeMux()
	registerAPIRoutes(apiMux, mgr, cfg)
	if pool != nil {
		registerAuthRoutes(apiMux, pool)
		registerUserRoutes(apiMux, pool)
		registerConfigRoutes(apiMux, pool, cfg)
		registerNotificationRoutes(apiMux, pool)
	}
	if actions != nil && actions.Store != nil {
		registerActionRoutes(
			apiMux, actions.Store, actions.Executor,
		)
	}
	if dbDeps != nil && dbDeps.Store != nil {
		registerDatabaseRoutes(apiMux, dbDeps)
	}

	// Stack middlewares onto API routes only.
	var apiHandler http.Handler = apiMux
	for i := len(middlewares) - 1; i >= 0; i-- {
		apiHandler = middlewares[i](apiHandler)
	}
	// Always apply body size limit and CORS to API routes.
	apiHandler = maxBodyMiddleware(apiHandler)
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
) {
	mux.HandleFunc(
		"GET /api/v1/databases", databasesHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/findings", findingsListHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/findings/{id}",
		findingDetailHandler(mgr))
	mux.HandleFunc(
		"POST /api/v1/findings/{id}/suppress",
		suppressHandler(mgr))
	mux.HandleFunc(
		"POST /api/v1/findings/{id}/unsuppress",
		unsuppressHandler(mgr))
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
	mux.HandleFunc(
		"PUT /api/v1/config",
		configUpdateHandler(mgr, cfg))
	mux.HandleFunc(
		"GET /api/v1/metrics", metricsHandler(mgr))
	mux.HandleFunc(
		"POST /api/v1/emergency-stop",
		emergencyStopHandler(mgr))
	mux.HandleFunc(
		"POST /api/v1/resume", resumeHandler(mgr))
}

func registerAuthRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool,
) {
	mux.HandleFunc(
		"POST /api/v1/auth/login", loginHandler(pool))
	mux.HandleFunc(
		"POST /api/v1/auth/logout", logoutHandler(pool))
	mux.HandleFunc(
		"GET /api/v1/auth/me", meHandler())
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
) {
	adminOnly := RequireRole("admin")
	cs := store.NewConfigStore(pool)

	globalGet := adminOnly(http.HandlerFunc(
		configGlobalGetHandler(cs, cfg)))
	mux.Handle("GET /api/v1/config/global", globalGet)

	globalPut := adminOnly(http.HandlerFunc(
		configGlobalPutHandler(cs, cfg)))
	mux.Handle("PUT /api/v1/config/global", globalPut)

	dbGet := adminOnly(http.HandlerFunc(
		configDBGetHandler(cs, cfg)))
	mux.Handle(
		"GET /api/v1/config/databases/{id}", dbGet)

	dbPut := adminOnly(http.HandlerFunc(
		configDBPutHandler(cs, cfg)))
	mux.Handle(
		"PUT /api/v1/config/databases/{id}", dbPut)

	audit := adminOnly(http.HandlerFunc(
		configAuditHandler(cs)))
	mux.Handle("GET /api/v1/config/audit", audit)
}

func registerActionRoutes(
	mux *http.ServeMux,
	as *store.ActionStore,
	exec *executor.Executor,
) {
	operatorUp := RequireRole("admin", "operator")

	pendingH := operatorUp(http.HandlerFunc(
		pendingActionsHandler(as)))
	mux.Handle(
		"GET /api/v1/actions/pending", pendingH)

	approveH := operatorUp(http.HandlerFunc(
		approveActionHandler(as, exec)))
	mux.Handle(
		"POST /api/v1/actions/{id}/approve", approveH)

	rejectH := operatorUp(http.HandlerFunc(
		rejectActionHandler(as)))
	mux.Handle(
		"POST /api/v1/actions/{id}/reject", rejectH)

	execH := operatorUp(http.HandlerFunc(
		manualExecuteHandler(exec)))
	mux.Handle(
		"POST /api/v1/actions/execute", execH)

	// Pending count available to all roles for nav badge.
	mux.HandleFunc(
		"GET /api/v1/actions/pending/count",
		pendingCountHandler(as))
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
	return d
}
