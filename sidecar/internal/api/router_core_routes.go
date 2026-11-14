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
	waitLoad LakebaseWaitLoadFunc,
) {
	registerFindingRoutes(mux, mgr)
	registerSourceFixRoutes(mux, mgr, cfg) // source_fix_routes.go (mcp-06)
	registerObservabilityRoutes(mux, mgr, waitLoad)
	registerProcessControlRoutes(mux, mgr, controller, cs, disableConfigWrites)
	registerLLMModelRoutes(mux, controller)
	registerIncidentRoutes(mux, mgr)
	registerAnalysisRoutes(mux, mgr, cfg, llmMgr)
	registerEventRoutes(mux, mgr)
}

// registerFindingRoutes covers findings and cases.
func registerFindingRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET /api/v1/databases", gateViewer, mgr,
		aggregateHandler(databasesHandler))
	routeAggregate(mux, "GET /api/v1/findings", gateViewer, mgr,
		aggregateHandler(findingsListHandler))
	routeAggregate(mux, "GET /api/v1/findings/{id}", gateViewer, mgr,
		aggregateHandler(findingDetailHandler))
	route(mux, "GET /api/v1/findings/categories", gateViewer,
		http.HandlerFunc(findingCategoriesHandler))
	routeAggregate(mux, "GET /api/v1/cases", gateViewer, mgr, aggregateHandler(casesHandler))

	suppressH := http.HandlerFunc(
		suppressHandler(mgr))
	routeDB(mux, "POST /api/v1/findings/{id}/suppress", gateOperator, queryDatabase, suppressH)

	unsuppressH := http.HandlerFunc(
		unsuppressHandler(mgr))
	routeDB(mux, "POST /api/v1/findings/{id}/unsuppress", gateOperator, queryDatabase,
		unsuppressH)
}

// registerObservabilityRoutes covers read-only fleet state: actions,
// recommendations, forecasts, snapshots, fleet health and admission;
// waitLoad feeds the wait-load chart's Lakebase source (market-01b).
func registerObservabilityRoutes(
	mux *http.ServeMux, mgr *fleet.DatabaseManager, waitLoad LakebaseWaitLoadFunc,
) {
	routeAggregate(mux, "GET /api/v1/actions", gateViewer, mgr,
		aggregateHandler(actionsListHandler))
	routeAggregate(mux, "GET /api/v1/actions/{id}", gateViewer, mgr,
		aggregateHandler(actionDetailHandler))
	routeAggregate(mux, "GET /api/v1/action-outcomes", gateViewer, mgr,
		aggregateHandler(actionOutcomesHandler))
	routeAggregate(mux, "GET /api/v1/tuning/calibration", gateViewer, mgr,
		aggregateHandler(tuningCalibrationHandler))
	registerRecommendationRoutes(mux, mgr)
	routeAggregate(mux, "GET /api/v1/forecasts", gateViewer, mgr,
		aggregateHandler(forecastsHandler))
	routeAggregate(mux, "GET /api/v1/query-hints", gateViewer, mgr,
		aggregateHandler(queryHintsHandler))
	routeAggregate(mux, "GET /api/v1/alert-log", gateViewer, mgr, aggregateHandler(alertLogHandler))
	routeAggregate(mux, "GET /api/v1/snapshots/latest", gateViewer, mgr,
		aggregateHandler(snapshotLatestHandler))
	routeAggregate(mux, "GET /api/v1/snapshots/history", gateViewer, mgr,
		aggregateHandler(snapshotHistoryHandler))
	registerWorkloadRoutes(mux, mgr, waitLoad) // workload_routes.go (market-01, market-01b)
	// v0.12 — Fleet-wide health time-series (ui-redesign-v2 §5 Overview).
	routeAggregate(mux, "GET /api/v1/fleet/health", gateViewer, mgr,
		aggregateHandler(fleetHealthHandler))
	routeAggregate(mux, "GET /api/v1/fleet/readiness", gateViewer, mgr,
		aggregateHandler(fleetReadinessHandler))
	routeAggregate(mux, "GET /api/v1/admission", gateViewer, mgr,
		aggregateHandler(admissionListHandler))
	routeAggregate(mux, "GET /api/v1/admission/{name}", gateViewer, mgr,
		aggregateHandler(admissionStatusHandler))
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

	routeAggregate(mux, "GET /api/v1/metrics", gateViewer, mgr, aggregateHandler(metricsHandler))

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
	routeAggregate(mux, "GET /api/v1/incidents", gateViewer, mgr,
		aggregateHandler(incidentsListHandler))
	routeAggregate(mux, "GET /api/v1/incidents/active", gateViewer, mgr,
		aggregateHandler(incidentsActiveHandler))
	routeAggregate(mux, "GET /api/v1/incidents/{id}", gateViewer, mgr,
		aggregateHandler(incidentDetailHandler))
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
	routeAggregate(mux, "GET /api/v1/forecasts/growth", gateViewer, mgr,
		aggregateHandler(growthForecastHandler))

	// v0.11 — Findings stats aggregate (replaces the old
	// /api/v1/schema/findings + /stats split). Schema-lint rows
	// live in sage.findings under category LIKE 'schema_lint:%';
	// callers pass source=schema_lint to filter to that subsystem.
	routeAggregate(mux, "GET /api/v1/findings/stats", gateViewer, mgr,
		aggregateHandler(findingsStatsHandler))
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
	routeRead(mux, "GET /api/v1/events", gateViewer, readStream,
		http.HandlerFunc(eventsHandler(defaultBroker)))
}
