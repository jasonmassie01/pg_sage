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
	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
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

// RuntimeDeps carries process-scoped controllers that are optional for
// embedders and tests but required by the production sidecar.
type RuntimeDeps struct {
	ConfigController *config.ConfigController
	ConfigBase       *config.Config
	// ConfigBaseLoader reloads the current file config (no overrides) so
	// override deletes rebase on it instead of the startup clone (G5-B03).
	ConfigBaseLoader    func() (*config.Config, error)
	DisableConfigWrites bool
	MCPHandler          http.Handler
	// LLMBudgets covers every LLM client (general, optimizer, per-database)
	// and the fleet budget; nil falls back to the shared manager (G3-B14).
	LLMBudgets LLMBudgetRegistry
	// NotificationSecretKey seals channel secrets at rest; every runtime
	// dispatcher must read with the same key (G7-B20). nil = plaintext.
	NotificationSecretKey []byte
	// NotificationTargetPolicy validates channel targets on write and at
	// test-send time (G7-B21).
	NotificationTargetPolicy notify.TargetPolicy
}

// NewRouterFullRuntime creates the API handler with process controllers.
func NewRouterFullRuntime(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	actions *ActionDeps,
	dbDeps *DatabaseDeps,
	llmMgr *llm.Manager,
	runtime *RuntimeDeps,
	middlewares ...func(http.Handler) http.Handler,
) http.Handler {
	apiMux := http.NewServeMux()
	var rt RuntimeDeps
	if runtime != nil {
		rt = *runtime
	}
	controller := rt.ConfigController
	disableConfigWrites := rt.DisableConfigWrites
	mcpHandler := rt.MCPHandler
	var runtimeConfigStore *store.ConfigStore
	if pool != nil && !disableConfigWrites {
		runtimeConfigStore = store.NewConfigStore(pool)
	}
	registerAPIRoutes(
		apiMux, mgr, cfg, llmMgr, controller, runtimeConfigStore,
		disableConfigWrites,
	)
	registerLLMBudgetRoutes(apiMux, llmBudgetSource(rt.LLMBudgets, llmMgr))
	// Sage SRE investigations live with each database's runtime (D3-style
	// fleet resolution), not in the control pool.
	registerSRERoutes(apiMux, mgr)
	registerSRESignalRoutes(apiMux, mgr, cfg)
	if cfg != nil && cfg.MCP.Enabled && cfg.MCP.Transport == "http" &&
		mcpHandler != nil {
		apiMux.Handle("POST /api/v1/mcp", bindMCPPrincipal(mcpHandler))
	}
	// Value is read from every monitored database in all modes (D3), so
	// it depends on the fleet, not on the control pool.
	apiMux.Handle("GET /api/v1/value", valueHandler(fleetValueReader(mgr)))
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
		registerConfigRoutesRuntime(
			apiMux, pool, cfg, mgr, controller,
			runtimeConfigBase(rt.ConfigBaseLoader, rt.ConfigBase, cfg),
			disableConfigWrites,
		)
		registerNotificationRoutes(apiMux, pool, notificationRouteDeps{
			secretKey: rt.NotificationSecretKey,
			policy:    rt.NotificationTargetPolicy,
		})
		registerPolicyRoutes(apiMux, policy.NewStore(pool))
		registerAgentDBRoutesWithAuthority(
			apiMux,
			agentdb.NewStore(pool),
			newAgentDBBlueprintGenerator(llmMgr),
			newAgentDBLiveAuthority(cfg.AgentDB),
		)
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

	// Unauthenticated liveness endpoint. It was in the auth-skip
	// allowlist but never registered, so /health fell through to the
	// SPA and returned index.html instead of a real health check (W2).
	root.HandleFunc("/health", func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

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
	controller *config.ConfigController,
	cs *store.ConfigStore,
	disableConfigWrites bool,
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
	registerRecommendationRoutes(mux, mgr)
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
		"GET /api/v1/admission", admissionListHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/admission/{name}",
		admissionStatusHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/config", configGetHandler(mgr, cfg, controller))

	configPutHandler := configUpdateHandlerWithStore(
		mgr, cfg, controller, cs,
	)
	if disableConfigWrites {
		configPutHandler = configPersistenceUnavailableHandler()
	}
	configPutH := adminOnly(http.HandlerFunc(configPutHandler))
	mux.Handle("PUT /api/v1/config", configPutH)

	mux.HandleFunc(
		"GET /api/v1/metrics", metricsHandler(mgr))

	stopH := operatorUp(http.HandlerFunc(
		emergencyStopHandler(mgr)))
	mux.Handle("POST /api/v1/emergency-stop", stopH)

	resumeH := operatorUp(http.HandlerFunc(
		resumeHandler(mgr)))
	mux.Handle("POST /api/v1/resume", resumeH)

	// Restart the sidecar process so startup-only settings (trust tiers,
	// maintenance window, execution mode, intervals) take effect. Requires
	// a supervisor (launcher loop / orchestrator) that relaunches on the
	// restart exit code; returns 501 if no restart hook is wired.
	mux.Handle("POST /api/v1/restart",
		adminOnly(http.HandlerFunc(restartHandler)))

	mux.HandleFunc(
		"GET /api/v1/llm/models",
		listModelsHandler(&cfg.LLM, controller))
	mux.Handle(
		"POST /api/v1/llm/models",
		adminOnly(http.HandlerFunc(discoverModelsHandler(&cfg.LLM, controller))))
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
		oauthAuthorizeRouter(oauthProvider, pool))
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/callback",
		oauthCallbackHandler(
			oauthProvider, pool,
			cfg.OAuth.DefaultRole, cfg.OAuth.Provider))
	registerAccountLinkRoutes(mux, pool, oauthProvider, cfg.OAuth.Provider)
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

func registerConfigRoutesRuntime(
	mux *http.ServeMux,
	pool *pgxpool.Pool,
	cfg *config.Config,
	fm *fleet.DatabaseManager,
	controller *config.ConfigController,
	base configBaseSource,
	disableWrites bool,
) {
	adminOnly := RequireRole("admin")
	if disableWrites {
		unavailable := adminOnly(http.HandlerFunc(
			configPersistenceUnavailableHandler(),
		))
		globalGet := adminOnly(http.HandlerFunc(
			configReadOnlyGlobalGetHandler(cfg, controller),
		))
		mux.Handle("GET /api/v1/config/global", globalGet)
		mux.Handle("PUT /api/v1/config/global", unavailable)
		mux.Handle("DELETE /api/v1/config/global/{key}", unavailable)
		dbGet := adminOnly(http.HandlerFunc(
			configReadOnlyDBGetHandler(cfg, fm, controller),
		))
		mux.Handle("GET /api/v1/config/databases/{id}", dbGet)
		mux.Handle("PUT /api/v1/config/databases/{id}", unavailable)
		mux.Handle("DELETE /api/v1/config/databases/{id}/{key}", unavailable)
		mux.Handle("GET /api/v1/config/audit", unavailable)
		return
	}
	cs := store.NewConfigStore(pool)
	baseCfg, err := base()
	if err != nil {
		slog.Error("config base unavailable for read handlers", "error", err)
		baseCfg = config.Clone(cfg)
	}

	globalGet := adminOnly(http.HandlerFunc(
		configGlobalGetHandler(cs, baseCfg, controller)))
	mux.Handle("GET /api/v1/config/global", globalGet)

	globalPutHandler := configGlobalPutHandler(cs, cfg, fm, controller)
	globalPut := adminOnly(http.HandlerFunc(globalPutHandler))
	mux.Handle("PUT /api/v1/config/global", globalPut)

	globalDeleteHandler := configGlobalDeleteHandler(
		cs, cfg, base, fm, controller,
	)
	globalDelete := adminOnly(http.HandlerFunc(globalDeleteHandler))
	mux.Handle("DELETE /api/v1/config/global/{key}", globalDelete)

	dbGet := adminOnly(http.HandlerFunc(
		configDBGetHandler(cs, baseCfg, pool)))
	mux.Handle(
		"GET /api/v1/config/databases/{id}", dbGet)

	dbPutHandler := configDBPutHandler(cs, cfg, pool, fm)
	dbPut := adminOnly(http.HandlerFunc(dbPutHandler))
	mux.Handle(
		"PUT /api/v1/config/databases/{id}", dbPut)

	dbDeleteHandler := configDBDeleteHandler(cs, cfg, fm, controller)
	dbDelete := adminOnly(http.HandlerFunc(dbDeleteHandler))
	mux.Handle(
		"DELETE /api/v1/config/databases/{id}/{key}",
		dbDelete)

	audit := adminOnly(http.HandlerFunc(
		configAuditHandler(cs)))
	mux.Handle("GET /api/v1/config/audit", audit)
}

func configPersistenceUnavailableHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w,
			"persistent config API is unavailable in YAML fleet mode; edit the YAML file",
			http.StatusServiceUnavailable,
		)
	}
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

// notificationRouteDeps carries the channel secret key and target policy
// the runtime dispatchers use, so API writes and runtime reads agree.
type notificationRouteDeps struct {
	secretKey []byte
	policy    notify.TargetPolicy
}

func registerNotificationRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool, deps notificationRouteDeps,
) {
	adminOnly := RequireRole("admin")
	d := newDefaultDispatcher(pool, deps)
	ns := store.NewNotificationStore(pool, d).
		WithSecretKey(deps.secretKey).WithTargetPolicy(deps.policy)

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
	pool *pgxpool.Pool, deps notificationRouteDeps,
) *notify.Dispatcher {
	logFn := func(_, _ string, _ ...any) {}
	d := notify.NewDispatcherWithStore(
		notify.NewPoolStore(pool, deps.secretKey), logFn,
	)
	d.RegisterSender(notify.NewSlackSenderWithPolicy(deps.policy))
	d.RegisterSender(notify.NewEmailSenderWithPolicy(deps.policy))
	d.RegisterSender(notify.NewPagerDutySender())
	return d
}
