package api

import (
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// registerAPIRoutes registers the fleet-backed API routes every router
// serves, grouped by area below.
func registerAPIRoutes(
	mux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	llmMgr *llm.Manager,
	controller *config.ConfigController,
	cs *store.ConfigStore,
	disableConfigWrites bool,
) {
	registerFindingRoutes(mux, mgr)
	registerObservabilityRoutes(mux, mgr)
	registerProcessControlRoutes(mux, mgr, cfg, controller, cs, disableConfigWrites)
	registerLLMModelRoutes(mux, cfg, controller)
	registerIncidentRoutes(mux, mgr)
	registerAnalysisRoutes(mux, mgr, cfg, llmMgr)
	registerEventRoutes(mux, mgr)
}

// registerFindingRoutes covers findings, cases and the shadow report.
func registerFindingRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
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
}

// registerObservabilityRoutes covers read-only fleet state: actions,
// recommendations, forecasts, snapshots, fleet health and admission.
func registerObservabilityRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	mux.HandleFunc(
		"GET /api/v1/actions", actionsListHandler(mgr))
	mux.HandleFunc(
		"GET /api/v1/actions/{id}",
		actionDetailHandler(mgr))
	mux.HandleFunc("GET /api/v1/action-outcomes", actionOutcomesHandler(mgr))
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
}

// registerProcessControlRoutes covers the process-level config document,
// metrics, emergency stop/resume and restart.
func registerProcessControlRoutes(
	mux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	controller *config.ConfigController,
	cs *store.ConfigStore,
	disableConfigWrites bool,
) {
	adminOnly := RequireRole("admin")
	operatorUp := RequireRole("admin", "operator")

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
}

// registerLLMModelRoutes covers model listing and admin model discovery.
func registerLLMModelRoutes(
	mux *http.ServeMux, cfg *config.Config, controller *config.ConfigController,
) {
	adminOnly := RequireRole("admin")
	mux.HandleFunc(
		"GET /api/v1/llm/models",
		listModelsHandler(&cfg.LLM, controller))
	mux.Handle(
		"POST /api/v1/llm/models",
		adminOnly(http.HandlerFunc(discoverModelsHandler(&cfg.LLM, controller))))
}

// registerIncidentRoutes covers the v0.9 incident endpoints.
func registerIncidentRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	operatorUp := RequireRole("admin", "operator")
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
}

// registerAnalysisRoutes covers explain, growth forecasts and finding stats.
func registerAnalysisRoutes(
	mux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	llmMgr *llm.Manager,
) {
	operatorUp := RequireRole("admin", "operator")

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
}

// registerEventRoutes registers the SSE live-update stream. The broker
// starts once per process.
func registerEventRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
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
