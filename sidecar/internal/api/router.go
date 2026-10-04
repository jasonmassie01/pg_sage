package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"

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
	// Autonomy serves the Sage SRE earned-autonomy routes (M7); nil omits.
	Autonomy *AutonomyDeps
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
	registerFleetScopedRoutes(apiMux, mgr, cfg, pool, llmMgr, rt)
	if pool != nil {
		registerControlPoolRoutes(apiMux, mgr, cfg, pool, llmMgr, rt)
	}
	if actions != nil && (actions.Store != nil ||
		actions.Fleet != nil) {
		registerActionRoutes(apiMux, actions)
	}
	if mgr != nil {
		registerApprovalCardRoutes(apiMux, mgr, autonomyRegistry(rt.Autonomy))
	}
	if dbDeps != nil && dbDeps.Store != nil {
		registerDatabaseRoutes(apiMux, dbDeps)
	}
	apiHandler := wrapAPIHandler(apiMux, middlewares)

	// Top-level mux: API routes get auth, static does not.
	root := http.NewServeMux()
	root.Handle("/api/v1/", apiHandler)
	if pool != nil {
		// Signed ChatOps callbacks bypass session auth (their provider
		// signature authenticates them); identity mapping stays admin-only.
		registerChatOpsRoutes(root, apiMux, pool, mgr, cfg, rt)
	}
	registerRootRoutes(root)
	return root
}

// registerFleetScopedRoutes registers the routes that resolve their data
// through the fleet and therefore exist without a control pool.
func registerFleetScopedRoutes(
	apiMux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	llmMgr *llm.Manager,
	rt RuntimeDeps,
) {
	var runtimeConfigStore *store.ConfigStore
	if pool != nil && !rt.DisableConfigWrites {
		runtimeConfigStore = store.NewConfigStore(pool)
	}
	registerAPIRoutes(
		apiMux, mgr, cfg, llmMgr, rt.ConfigController, runtimeConfigStore,
		rt.DisableConfigWrites,
	)
	registerLLMBudgetRoutes(apiMux, llmBudgetSource(rt.LLMBudgets, llmMgr))
	// Sage SRE investigations live with each database's runtime (D3-style
	// fleet resolution), not in the control pool.
	registerSRERoutes(apiMux, mgr, autonomyRegistry(rt.Autonomy))
	registerSRESignalRoutes(apiMux, mgr, cfg)
	registerAutonomyRoutes(apiMux, mgr, rt.Autonomy)
	registerTrustRoutes(apiMux, mgr, rt.Autonomy)
	registerShadowRoutes(apiMux, mgr)
	registerFactRoutes(apiMux, mgr)
	registerModelLiftRoutes(apiMux, rt.Autonomy)
	if cfg != nil && cfg.MCP.Enabled && cfg.MCP.Transport == "http" &&
		rt.MCPHandler != nil {
		apiMux.Handle("POST /api/v1/mcp",
			bindMCPPrincipal(rt.MCPHandler, mcpTokenStore(pool)))
	}
	// Value is read from every monitored database in all modes (D3), so
	// it depends on the fleet, not on the control pool.
	apiMux.Handle("GET /api/v1/value", valueHandler(fleetValueReader(mgr)))
}

// registerControlPoolRoutes registers the routes backed by the control
// database: auth, users, persistent config, notifications, policy and
// agent databases.
func registerControlPoolRoutes(
	apiMux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	llmMgr *llm.Manager,
	rt RuntimeDeps,
) {
	registerAuthRoutes(apiMux, pool, newRouterOAuthProvider(cfg), cfg)
	registerUserRoutes(apiMux, pool)
	registerMCPTokenRoutes(apiMux, pool)
	registerConfigRoutesRuntime(
		apiMux, pool, cfg, mgr, rt.ConfigController,
		runtimeConfigBase(rt.ConfigBaseLoader, rt.ConfigBase, cfg),
		rt.DisableConfigWrites,
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

// newRouterOAuthProvider discovers the configured OAuth provider and starts
// its CSRF-state cleaner; nil when OAuth is disabled or discovery fails.
func newRouterOAuthProvider(cfg *config.Config) *auth.OAuthProvider {
	if !cfg.OAuth.Enabled {
		return nil
	}
	oauthProvider := auth.NewOAuthProvider(&cfg.OAuth)
	if err := oauthProvider.Discover(
		context.Background(),
	); err != nil {
		slog.Error("oauth discovery failed",
			"error", err)
		return nil
	}
	go oauthProvider.StartStateCleaner(
		shutdownContext())
	return oauthProvider
}

// wrapAPIHandler stacks the caller middlewares onto the API mux, then the
// fixed chain every API route gets.
func wrapAPIHandler(
	apiMux *http.ServeMux, middlewares []func(http.Handler) http.Handler,
) http.Handler {
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
	return apiHandler
}

// registerRootRoutes registers the unauthenticated liveness endpoint and
// the embedded dashboard on the top-level mux.
func registerRootRoutes(root *http.ServeMux) {
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
}
