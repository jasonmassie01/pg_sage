package api

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/mcpauth"
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
	// DatabricksAppAuth disables local login/account recovery in an App.
	DatabricksAppAuth bool
	ConfigController  *config.ConfigController
	ConfigBase        *config.Config
	// ConfigBaseLoader reloads the current file config (no overrides) so
	// override deletes rebase on it instead of the startup clone (G5-B03).
	ConfigBaseLoader    func() (*config.Config, error)
	DisableConfigWrites bool
	MCPHandler          http.Handler
	// MCPOAuth validates OAuth 2.1 access tokens on the MCP endpoints (E2);
	// nil accepts pg_sage's own MCP tokens only.
	MCPOAuth *mcpauth.Validator
	// Audit serves the E2 audit routes; its Pool, Control and DatabaseID are
	// filled from the fleet and the control pool.
	Audit AuditDeps
	// WorkloadPseudonymKey keys the workload pack model tier's identifier
	// hashing (evid-02c); nil refuses hash_identifiers.
	WorkloadPseudonymKey []byte
	// LLMBudgets covers every LLM client (general, optimizer, per-database)
	// and the fleet budget; nil falls back to the shared manager (G3-B14).
	LLMBudgets LLMBudgetRegistry
	// TelemetryStatus must return a sanitized, read-only status projection:
	// never event payloads, SQL, credentials, or SDK response objects.
	TelemetryStatus func() any
	// TelemetryHistory resolves a registered database to retained normalized
	// history. It must never run caller-supplied SQL or broaden provider scope.
	TelemetryHistory func(context.Context, string, string, time.Time, time.Time, int) (any, error)
	// LakebaseWaitLoad reads a database's raw Lakebase wait counters as the
	// wait-load chart's preferred source after Performance Insights
	// (market-01b); nil keeps the activity samples alone.
	LakebaseWaitLoad LakebaseWaitLoadFunc
	// LakebaseCredentials lists each Lakebase endpoint's credential refresh
	// state for the admin deployment status; nil reports no endpoints.
	LakebaseCredentials func() []databricksauth.EndpointCredential
	// LakebaseEndpointInfo reports an endpoint's suspend timeout, compute
	// state and the pg_sage cadences that may keep it active (databricks-04);
	// nil reports them as unknown.
	LakebaseEndpointInfo func(string) (databricksauth.EndpointInfo, bool)
	// NotificationSecretKey seals channel secrets at rest; every runtime
	// dispatcher must read with the same key (G7-B20). nil = plaintext.
	NotificationSecretKey []byte
	// NotificationPreviousSecretKey also opens channel secrets sealed under
	// the previous key of a rotation (security-08). nil = none.
	NotificationPreviousSecretKey []byte
	// SecretsPosture reports how stored secrets are protected for the
	// admin banner (security-08); nil omits GET /api/v1/security/posture.
	SecretsPosture func(context.Context) (SecretsPosture, error)
	// NotificationTargetPolicy validates channel targets on write and at
	// test-send time (G7-B21).
	NotificationTargetPolicy notify.TargetPolicy
	// CaseTicketSync starts the case ticket sync worker on the shutdown
	// context (market-14); tests leave it off.
	CaseTicketSync bool
	// Autonomy serves the Sage SRE earned-autonomy routes (M7); nil omits.
	Autonomy *AutonomyDeps
	// Ask serves the Ask Sage routes (roadmap phase 3); nil omits them.
	Ask *ask.Registry
	// Specialist serves the Postgres-specialist contract under
	// /api/v1/specialist/ (MCP tokens only); nil omits it. SpecialistAudit
	// reads its request audit for the dashboard; nil omits the route.
	Specialist      http.Handler
	SpecialistAudit SpecialistAuditReader
	// FleetLearning serves look-alikes and the leader status (fleet
	// learning); nil answers 503 / "election disabled".
	FleetLearning FleetLearningReader
	// AgentEnvironments serves agent environment labels; nil answers 503
	// (no control database: agent governance is posture-only).
	AgentEnvironments *envbind.Service
	// AgentKill serves the agent kill switch, freeze and unfreeze; nil
	// answers 503 (agent_kill_routes.go).
	AgentKill AgentKillSwitch
	// AgentActivity serves an agent's activity (G1-10); nil answers 503.
	AgentActivity AgentActivityReader
	// AgentGrants serves agent grants and capability requests; nil answers
	// 503 (no control database or executor).
	AgentGrants AgentGrantService
	// TLSExpiring adds tls_expiring to /ready details; nil when the API
	// serves plain HTTP.
	TLSExpiring func() bool
	// Metrics serves GET /metrics on this listener (prometheus.serve_on_api,
	// observability-08); nil leaves the path to the dashboard.
	Metrics *MetricsRoute
	// DatabaseGrants reads per-database grants when api.database_grants is
	// on (apiauth-08); nil reads sage.user_database_grants on the control
	// pool.
	DatabaseGrants DatabaseGrantSource
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
) (http.Handler, error) {
	if runtime == nil || runtime.ConfigController == nil {
		return nil, errors.New("api router: a config controller is required " +
			"(every mode builds one before the API starts)")
	}
	apiMux := http.NewServeMux()
	rt := *runtime
	registerAuthModeRoute(apiMux, rt.DatabricksAppAuth)
	registerFleetScopedRoutes(apiMux, mgr, cfg, pool, llmMgr, rt)
	if pool != nil {
		registerControlPoolRoutes(apiMux, mgr, cfg, pool, rt)
	}
	if actions != nil && (actions.Store != nil || actions.Fleet != nil) {
		registerActionRoutes(apiMux, actions)
	}
	if mgr != nil {
		registerApprovalCardRoutes(apiMux, mgr, autonomyRegistry(rt.Autonomy))
	}
	if dbDeps != nil && dbDeps.Store != nil {
		registerDatabaseRoutes(apiMux, dbDeps)
	}
	registerAuditRoutes(apiMux, fleetAuditDeps(rt.Audit, mgr, pool))
	if rt.DatabricksAppAuth {
		// Innermost: verify platform identity before reporting unsupported
		// optional protocols. No second token system is mounted in this mode.
		middlewares = append(append([]func(http.Handler) http.Handler{}, middlewares...), databricksAppProtocolGuard)
	}
	authz := routerDatabaseAuthz(cfg, pool, rt.DatabaseGrants)
	apiHandler := wrapAPIHandler(withDatabaseAuthz(authz, apiMux), middlewares,
		newCrossOriginProtection(cfg, rt.DatabricksAppAuth), askRequestTimeout(cfg),
		accessLogOn(cfg))

	// Top-level mux: API routes get auth, static does not.
	root := http.NewServeMux()
	mount(root, "/api/v1/", apiMux, apiHandler)
	if rt.DatabricksAppAuth {
		// Avoid a misleading SPA response for the common protocol alias.
		mount(root, "/mcp", apiMux, apiHandler)
		mount(root, "/mcp/", apiMux, apiHandler)
	}
	if pool != nil && !rt.DatabricksAppAuth {
		// Signed ChatOps callbacks bypass session auth (their provider
		// signature authenticates them); identity mapping stays admin-only.
		registerChatOpsRoutes(root, apiMux, pool, mgr, cfg, rt)
	}
	// Readiness (E1): config loaded, control database up, schema migrated.
	route(root, "/ready", gatePublic, NewReadinessHandler(routerReadiness(cfg, pool, rt)))
	registerMetricsRoute(root, rt.Metrics, pool, rt.DatabricksAppAuth)
	if rt.MCPHandler != nil && !rt.DatabricksAppAuth {
		registerMCPMetadata(root, rt.MCPOAuth)
	}
	registerRootRoutes(root)
	return root, nil
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
		rt.DisableConfigWrites, rt.LakebaseWaitLoad,
	)
	registerLLMBudgetRoutes(apiMux, llmBudgetSource(rt.LLMBudgets, llmMgr))
	registerDeploymentStatusRoutes(apiMux, cfg, rt, llmBudgetSource(rt.LLMBudgets, llmMgr))
	// Sage SRE investigations live with each database's runtime (D3-style
	// fleet resolution), not in the control pool.
	registerSRERoutes(apiMux, mgr, autonomyRegistry(rt.Autonomy), cfg,
		WorkloadPackKeys{Signing: rt.Audit.EvidenceKey, Pseudonym: rt.WorkloadPseudonymKey,
			Control: pool})
	registerSRESignalRoutes(apiMux, mgr, cfg)
	if rt.DatabricksAppAuth {
		registerAppSLISampleRoute(apiMux, mgr, cfg)
	}
	registerAutonomyRoutes(apiMux, mgr, rt.Autonomy)
	registerTrustRoutes(apiMux, mgr, rt.Autonomy)
	registerSelfAwareRoutes(apiMux, mgr)
	registerFactRoutes(apiMux, mgr)
	registerAgentClassRoutes(apiMux, mgr)
	registerAgentEnvRoutes(apiMux, rt.AgentEnvironments)
	registerAgentKillRoutes(apiMux, rt.AgentKill)
	registerAgentActivityRoutes(apiMux, rt.AgentActivity)
	registerAgentGrantRoutes(apiMux, rt.AgentGrants)
	registerFleetLearningRoutes(apiMux, mgr, cfg, rt.FleetLearning)
	registerManagedCloudRoutes(apiMux, mgr)
	registerProviderAdviceRoutes(apiMux, mgr)
	registerModelLiftRoutes(apiMux, rt.Autonomy)
	registerAskRoutes(apiMux, rt.Ask)
	registerPprofRoutes(apiMux, cfg != nil && cfg.Debug.PprofEnabled)
	registerSpecialistRoutes(apiMux, rt)
	if !rt.DatabricksAppAuth && cfg != nil && cfg.MCP.Enabled && cfg.MCP.Transport == "http" &&
		rt.MCPHandler != nil {
		registerMCPRoutes(apiMux, rt.MCPHandler, mcpTokenStore(pool), rt.MCPOAuth,
			agentguard.NewStore(pool))
	}
	registerFleetReadRoutes(apiMux, mgr, cfg, pool, rt.DatabricksAppAuth)
	registerOnboardingRoutes(apiMux, mgr, cfg, pool)
}

// registerFleetReadRoutes registers value and briefings (llm-06b), read
// from every monitored database in all modes (D3), and migration lint
// (migration-11), which reads one monitored database's catalogs: they
// depend on the fleet, not on the control pool. Outside a Databricks App
// migration lint also takes a read-scope API token from pool's store
// (migration-11b).
func registerFleetReadRoutes(apiMux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config, pool *pgxpool.Pool, appMode bool) {
	routeAggregate(apiMux, "GET /api/v1/value", gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler {
			return valueHandler(fleetValueReader(m, cfg))
		})
	registerBriefingRoutes(apiMux, mgr)
	if appMode {
		registerMigrationLintRoutes(apiMux, mgr, cfg)
	} else {
		registerMigrationLintTokenRoutes(apiMux, mgr, cfg,
			lintTokenStore(mcpTokenStore(pool)))
	}
	registerUpgradeReadinessRoutes(apiMux, mgr, cfg)
}

// registerControlPoolRoutes registers the routes backed by the control
// database: auth, users, persistent config, notifications, policy and the
// decommission inventory of the removed agent-database provisioner.
func registerControlPoolRoutes(
	apiMux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	pool *pgxpool.Pool,
	rt RuntimeDeps,
) {
	if rt.DatabricksAppAuth {
		registerDatabricksAppAuthRoutes(apiMux, pool)
	} else {
		registerAuthRoutes(apiMux, pool, newRouterOAuthProvider(cfg), cfg)
		registerUserRoutes(apiMux, pool)
	}
	registerDatabaseGrantRoutes(apiMux, pool, mgr,
		newDatabaseAuthz(cfg, pool, rt.DatabaseGrants) != nil,
		!rt.DatabricksAppAuth && auth.DatabaseGrantMappingConfigured(&cfg.OAuth))
	if !rt.DatabricksAppAuth {
		registerMCPTokenRoutes(apiMux, pool)
	}
	registerAgentRoutes(apiMux, pool, cfg)
	registerAgentIdentityRoutes(apiMux, pool, cfg)
	registerConfigRoutesRuntime(
		apiMux, pool, cfg, mgr, rt.ConfigController,
		runtimeConfigBase(rt.ConfigBaseLoader, rt.ConfigBase, cfg),
		rt.DisableConfigWrites,
	)
	notifyDeps := newNotificationRouteDeps(rt)
	notifyDeps.fleet = mgr
	ns := registerNotificationRoutes(apiMux, pool, notifyDeps)
	registerCaseTicketRoutes(apiMux, mgr, pool, ns, notifyDeps)
	if rt.CaseTicketSync {
		startCaseTicketSync(mgr, pool, ns, notifyDeps)
	}
	if !rt.DatabricksAppAuth {
		registerBreakGlassRoutes(apiMux, pool, cfg, newDefaultDispatcher(pool, notifyDeps))
	}
	registerPolicyRoutesWith(apiMux, policy.NewStore(pool), func() bool {
		return cfg != nil && cfg.Agents.SingleOperatorMode
	})
	registerDecommissionRoutes(apiMux, pool)
	registerSecurityPostureRoutes(apiMux, rt.SecretsPosture)
}

// newRouterOAuthProvider builds the configured OAuth provider, tries
// discovery once so a healthy IdP is ready at startup, and keeps
// discovery and the CSRF-state cleaner running on the shutdown context.
// It is nil only when OAuth is disabled: a failed discovery leaves SSO
// enabled but not ready until a retry succeeds.
func newRouterOAuthProvider(cfg *config.Config) *auth.OAuthProvider {
	if !cfg.OAuth.Enabled {
		routerSSOProvider.Store(nil)
		return nil
	}
	oauthProvider := auth.NewOAuthProvider(&cfg.OAuth)
	// Discover logs its own failure (WARN for an outage, one ERROR for a
	// misconfiguration) and StartDiscovery retries, so the error needs no
	// handling here.
	_ = oauthProvider.Discover(context.Background())
	routerSSOProvider.Store(oauthProvider)
	ctx := shutdownContext()
	go oauthProvider.StartDiscovery(ctx)
	go oauthProvider.StartStateCleaner(ctx)
	return oauthProvider
}

// wrapAPIHandler stacks the caller middlewares onto the API mux, then the
// fixed chain every API route gets; askTimeout is the ask route's request
// deadline (askRequestTimeout). accessLog adds the access log's two
// layers (access_log.go): one around the mux, one outside auth.
func wrapAPIHandler(
	apiMux http.Handler, middlewares []func(http.Handler) http.Handler,
	crossOrigin *http.CrossOriginProtection, askTimeout time.Duration, accessLog bool,
) http.Handler {
	// Stack middlewares onto API routes only.
	var apiHandler http.Handler = apiMux
	if accessLog {
		apiHandler = accessStampMiddleware(apiHandler)
	}
	for i := len(middlewares) - 1; i >= 0; i-- {
		apiHandler = middlewares[i](apiHandler)
	}
	// Always apply the cross-origin check, body size limit, security
	// headers, JSON content-type validation, and a per-request deadline
	// to API routes. The deadline is applied outside the body/JSON
	// middlewares so its context also covers body parsing. The
	// cross-origin check runs before any of them (and before auth), and
	// its refusal still carries the security headers.
	apiHandler = requireJSONMiddleware(apiHandler)
	apiHandler = maxBodyMiddleware(apiHandler)
	apiHandler = routeTimeoutMiddleware(apiHandler, askTimeout)
	apiHandler = crossOrigin.Handler(apiHandler)
	if accessLog {
		// Outside the cross-origin check and auth, so it logs their
		// refusals too; inside the security headers.
		apiHandler = accessLogMiddleware(apiHandler)
	}
	apiHandler = securityHeadersMiddleware(apiHandler)
	return apiHandler
}

// registerRootRoutes registers the unauthenticated liveness endpoint and
// the embedded dashboard on the top-level mux.
func registerRootRoutes(root *http.ServeMux) {
	// Unauthenticated liveness endpoint. It was in the auth-skip
	// allowlist but never registered, so /health fell through to the
	// SPA and returned index.html instead of a real health check (W2).
	route(root, "/health", gatePublic, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))

	// Embedded dashboard (SPA fallback), gzipped with cache validators.
	route(root, "/", gatePublic, embeddedDashboard())
}

// embeddedDashboard returns the static handler over the embedded dist. The
// embed is fixed at compile time and the handler is immutable, so it is built
// (hashed and gzipped) once per process, not on every NewRouter call.
var embeddedDashboard = sync.OnceValue(buildEmbeddedDashboard)

// buildEmbeddedDashboard indexes the embedded dist. A failure here is a broken
// build; it is logged and answered with 503 rather than silently serving nothing.
func buildEmbeddedDashboard() http.Handler {
	sub, err := fs.Sub(staticFiles, "dist")
	if err == nil {
		var h http.Handler
		if h, err = newStaticHandler(sub); err == nil {
			return h
		}
	}
	slog.Error("embedded dashboard unavailable", "error", err)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "dashboard assets unavailable", http.StatusServiceUnavailable)
	})
}
