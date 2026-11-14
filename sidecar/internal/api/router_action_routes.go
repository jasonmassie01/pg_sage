package api

import (
	"net/http"
)

// registerActionRoutes registers the action queue routes. Fleet mode
// resolves the pool per request; standalone uses the store and executor;
// without either, the decision routes answer 501.
func registerActionRoutes(
	mux *http.ServeMux,
	deps *ActionDeps,
) {
	registerPendingActionRoutes(mux, deps)

	switch {
	case deps.Store != nil && deps.Executor != nil:
		registerExecutorActionRoutes(mux, deps)
	case deps.Fleet != nil:
		registerFleetActionRoutes(mux, deps)
	default:
		registerUnavailableActionRoutes(mux)
	}
}

// registerPendingActionRoutes covers the pending queue and the per-finding
// lookup used by the inline action flow on the Findings page.
func registerPendingActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
) {
	if deps.Fleet != nil {
		// Fleet mode: dynamically resolve pool on each
		// request so delete/re-add cycles don't break.
		routeAggregate(mux, "GET /api/v1/actions/pending", gateOperator, deps.Fleet,
			aggregateHandler(fleetPendingActionsHandler))
		routeAggregate(mux, "GET /api/v1/actions/pending/count", gateOperator, deps.Fleet,
			aggregateHandler(fleetPendingCountHandler))
	} else {
		pendingH := http.HandlerFunc(
			pendingActionsHandler(deps.Store, deps.Executor))
		route(mux, "GET /api/v1/actions/pending", gateOperator, pendingH)
		countH := http.HandlerFunc(
			pendingCountHandler(deps.Store))
		route(mux, "GET /api/v1/actions/pending/count", gateOperator, countH)
	}

	// Inline action flow on the Findings page — per-finding
	// lookup of any pending queued actions. Works in both
	// fleet and standalone modes.
	byFindingH := http.HandlerFunc(
		findingPendingActionsHandler(deps))
	route(mux, "GET /api/v1/findings/{id}/pending-actions", gateOperator, byFindingH)
}

func registerExecutorActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
) {
	approveH := http.HandlerFunc(
		approveActionHandler(
			deps.Store, deps.Executor))
	routeDB(mux, "POST /api/v1/actions/{id}/approve", gateOperator, queryDatabase, approveH)

	rejectH := http.HandlerFunc(
		rejectActionHandler(deps.Store))
	routeDB(mux, "POST /api/v1/actions/{id}/reject", gateOperator, queryDatabase, rejectH)

	rollbackH := http.HandlerFunc(
		rollbackActionHandler(deps.Executor))
	routeDB(mux, "POST /api/v1/actions/{id}/rollback", gateOperator, queryDatabase, rollbackH)

	execH := http.HandlerFunc(
		manualExecuteHandler(deps.Executor))
	routeDB(mux, "POST /api/v1/actions/execute", gateOperator, bodyDatabase, execH)

	routeDB(mux, "POST /api/v1/actions/{id}/schedule", gateOperator, queryDatabase,
		scheduleRoute(deps.Store, deps.Executor, false))
	routeDB(mux, "POST /api/v1/actions/{id}/unschedule", gateOperator, queryDatabase,
		scheduleRoute(deps.Store, deps.Executor, true))
}

func registerFleetActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
) {
	approveH := http.HandlerFunc(
		fleetApproveActionHandler(deps.Fleet))
	routeDB(mux, "POST /api/v1/actions/{id}/approve", gateOperator, queryDatabase, approveH)

	rejectH := http.HandlerFunc(
		fleetRejectActionHandler(deps.Fleet))
	routeDB(mux, "POST /api/v1/actions/{id}/reject", gateOperator, queryDatabase, rejectH)

	rollbackH := http.HandlerFunc(
		fleetRollbackActionHandler(deps.Fleet))
	routeDB(mux, "POST /api/v1/actions/{id}/rollback", gateOperator, queryDatabase, rollbackH)

	execH := http.HandlerFunc(
		fleetManualExecuteHandler(deps.Fleet))
	routeDB(mux, "POST /api/v1/actions/execute", gateOperator, bodyDatabase, execH)

	routeDB(mux, "POST /api/v1/actions/{id}/schedule", gateOperator, queryDatabase,
		fleetScheduleRoute(deps.Fleet, false))
	routeDB(mux, "POST /api/v1/actions/{id}/unschedule", gateOperator, queryDatabase,
		fleetScheduleRoute(deps.Fleet, true))
}

func registerUnavailableActionRoutes(mux *http.ServeMux) {
	notImpl := http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			jsonError(w,
				"action approval not available",
				http.StatusNotImplemented)
		})
	routeDB(mux, "POST /api/v1/actions/{id}/approve", gateOperator, queryDatabase, notImpl)
	routeDB(mux, "POST /api/v1/actions/{id}/reject", gateOperator, queryDatabase, notImpl)
	routeDB(mux, "POST /api/v1/actions/{id}/rollback", gateOperator, queryDatabase, notImpl)
	routeDB(mux, "POST /api/v1/actions/execute", gateOperator, bodyDatabase, notImpl)
	routeDB(mux, "POST /api/v1/actions/{id}/schedule", gateOperator, queryDatabase, notImpl)
	routeDB(mux, "POST /api/v1/actions/{id}/unschedule", gateOperator, queryDatabase, notImpl)
}
