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
	registerSourceFixRoutes(mux, mgr, cfg) // source_fix_routes.go (mcp-06)
	registerObservabilityRoutes(mux, mgr)
	registerProcessControlRoutes(mux, mgr, controller, cs, disableConfigWrites)
	registerLLMModelRoutes(mux, controller)
	registerIncidentRoutes(mux, mgr)
	registerAnalysisRoutes(mux, mgr, cfg, llmMgr)
	registerEventRoutes(mux, mgr)
}

// registerFindingRoutes covers findings and cases.
func registerFindingRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	route(mux, "GET /api/v1/databases", gateViewer, http.HandlerFunc(databasesHandler(mgr)))
	route(mux, "GET /api/v1/findings", gateViewer, http.HandlerFunc(findingsListHandler(mgr)))
	route(mux, "GET /api/v1/findings/{id}", gateViewer, http.HandlerFunc(findingDetailHandler(mgr)))
	route(mux, "GET /api/v1/findings/categories", gateViewer,
		http.HandlerFunc(findingCategoriesHandler))
	route(mux, "GET /api/v1/cases", gateViewer, http.HandlerFunc(casesHandler(mgr)))

	suppressH := http.HandlerFunc(
		suppressHandler(mgr))
	routeDB(mux, "POST /api/v1/findings/{id}/suppress", gateOperator, queryDatabase, suppressH)

	unsuppressH := http.HandlerFunc(
		unsuppressHandler(mgr))
	routeDB(mux, "POST /api/v1/findings/{id}/unsuppress", gateOperator, queryDatabase,
		unsuppressH)
}

// registerObservabilityRoutes covers read-only fleet state: actions,
// recommendations, forecasts, snapshots, fleet health and admission.
func registerObservabilityRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	route(mux, "GET /api/v1/actions", gateViewer, http.HandlerFunc(actionsListHandler(mgr)))
	route(mux, "GET /api/v1/actions/{id}", gateViewer, http.HandlerFunc(actionDetailHandler(mgr)))
	route(mux, "GET /api/v1/action-outcomes", gateViewer,
		http.HandlerFunc(actionOutcomesHandler(mgr)))
	route(mux, "GET /api/v1/tuning/calibration", gateViewer,
		http.HandlerFunc(tuningCalibrationHandler(mgr)))
	registerRecommendationRoutes(mux, mgr)
	route(mux, "GET /api/v1/forecasts", gateViewer, http.HandlerFunc(forecastsHandler(mgr)))
	route(mux, "GET /api/v1/query-hints", gateViewer, http.HandlerFunc(queryHintsHandler(mgr)))
	route(mux, "GET /api/v1/alert-log", gateViewer, http.HandlerFunc(alertLogHandler(mgr)))
	route(mux, "GET /api/v1/snapshots/latest", gateViewer,
		http.HandlerFunc(snapshotLatestHandler(mgr)))
	route(mux, "GET /api/v1/snapshots/history", gateViewer,
		http.HandlerFunc(snapshotHistoryHandler(mgr)))
	// v0.12 — Fleet-wide health time-series (ui-redesign-v2 §5 Overview).
	route(mux, "GET /api/v1/fleet/health", gateViewer, http.HandlerFunc(fleetHealthHandler(mgr)))
	route(mux, "GET /api/v1/fleet/readiness", gateViewer,
		http.HandlerFunc(fleetReadinessHandler(mgr)))
	route(mux, "GET /api/v1/admission", gateViewer, http.HandlerFunc(admissionListHandler(mgr)))
	route(mux, "GET /api/v1/admission/{name}", gateViewer,
		http.HandlerFunc(admissionStatusHandler(mgr)))
}

// registerProcessControlRoutes covers the process-level config document,
// metrics, emergency stop/resume and restart.
func registerProcessControlRoutes(
	mux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	controller *config.ConfigController,
	cs *store.ConfigStore,
	disableConfigWrites bool,
) {
	route(mux, "GET /api/v1/config", gateViewer,
		http.HandlerFunc(configGetHandler(mgr, controller)))

	configPutHandler := configUpdateHandlerWithStore(controller, cs)
	if disableConfigWrites {
		configPutHandler = configPersistenceUnavailableHandler()
	}
	configPutH := http.HandlerFunc(configPutHandler)
	route(mux, "PUT /api/v1/config", gateAdmin, configPutH)

	route(mux, "GET /api/v1/metrics", gateViewer, http.HandlerFunc(metricsHandler(mgr)))

	stopH := http.HandlerFunc(
		emergencyStopHandler(mgr))
	routeDB(mux, "POST /api/v1/emergency-stop", gateOperator, queryDatabase, stopH)

	resumeH := http.HandlerFunc(
		resumeHandler(mgr))
	routeDB(mux, "POST /api/v1/resume", gateOperator, queryDatabase, resumeH)

	// Restart the sidecar process so startup-only settings (trust tiers,
	// maintenance window, execution mode, intervals) take effect. Requires
	// a supervisor (launcher loop / orchestrator) that relaunches on the
	// restart exit code; returns 501 if no restart hook is wired.
	route(mux, "POST /api/v1/restart", gateAdmin, http.HandlerFunc(restartHandler))
}

// registerLLMModelRoutes covers model listing and admin model discovery.
func registerLLMModelRoutes(
	mux *http.ServeMux, controller *config.ConfigController,
) {
	route(mux, "GET /api/v1/llm/models", gateViewer,
		http.HandlerFunc(listModelsHandler(controller)))
	route(mux, "POST /api/v1/llm/models", gateAdmin,
		http.HandlerFunc(discoverModelsHandler(controller)))
}

// registerIncidentRoutes covers the v0.9 incident endpoints.
func registerIncidentRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	route(mux, "GET /api/v1/incidents", gateViewer, http.HandlerFunc(incidentsListHandler(mgr)))
	route(mux, "GET /api/v1/incidents/active", gateViewer,
		http.HandlerFunc(incidentsActiveHandler(mgr)))
	route(mux, "GET /api/v1/incidents/{id}", gateViewer,
		http.HandlerFunc(incidentDetailHandler(mgr)))
	resolveH := http.HandlerFunc(
		incidentResolveHandler(mgr))
	routeDB(mux, "POST /api/v1/incidents/{id}/resolve", gateOperator, queryDatabase,
		resolveH)
}

// registerAnalysisRoutes covers explain, growth forecasts, finding stats
// and the decision ledger (decisions, policy explain).
func registerAnalysisRoutes(
	mux *http.ServeMux,
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
	llmMgr *llm.Manager,
) {
	// v0.9 — Explain endpoint
	var explainLLM *llm.Client
	if llmMgr != nil {
		explainLLM = llmMgr.General
	}
	explainH := http.HandlerFunc(
		explainHandler(mgr, cfg, explainLLM))
	routeDB(mux, "POST /api/v1/explain", gateOperator, queryDatabase, explainH)

	// v0.9 — Growth forecast endpoint
	route(mux, "GET /api/v1/forecasts/growth", gateViewer,
		http.HandlerFunc(growthForecastHandler(mgr)))

	// v0.11 — Findings stats aggregate (replaces the old
	// /api/v1/schema/findings + /stats split). Schema-lint rows
	// live in sage.findings under category LIKE 'schema_lint:%';
	// callers pass source=schema_lint to filter to that subsystem.
	route(mux, "GET /api/v1/findings/stats", gateViewer,
		http.HandlerFunc(findingsStatsHandler(mgr)))
	registerDecisionRoutes(mux, mgr) // policy-07
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
	route(mux, "GET /api/v1/events", gateViewer, http.HandlerFunc(eventsHandler(defaultBroker)))
}
