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
		pendingH := http.HandlerFunc(
			fleetPendingActionsHandler(deps.Fleet))
		route(mux, "GET /api/v1/actions/pending", gateOperator, pendingH)
		countH := http.HandlerFunc(
			fleetPendingCountHandler(deps.Fleet))
		route(mux, "GET /api/v1/actions/pending/count", gateOperator, countH)
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
	route(mux, "POST /api/v1/actions/{id}/approve", gateOperator, approveH)

	rejectH := http.HandlerFunc(
		rejectActionHandler(deps.Store))
	route(mux, "POST /api/v1/actions/{id}/reject", gateOperator, rejectH)

	rollbackH := http.HandlerFunc(
		rollbackActionHandler(deps.Executor))
	route(mux, "POST /api/v1/actions/{id}/rollback", gateOperator, rollbackH)

	execH := http.HandlerFunc(
		manualExecuteHandler(deps.Executor))
	route(mux, "POST /api/v1/actions/execute", gateOperator, execH)
}

func registerFleetActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
) {
	approveH := http.HandlerFunc(
		fleetApproveActionHandler(deps.Fleet))
	route(mux, "POST /api/v1/actions/{id}/approve", gateOperator, approveH)

	rejectH := http.HandlerFunc(
		fleetRejectActionHandler(deps.Fleet))
	route(mux, "POST /api/v1/actions/{id}/reject", gateOperator, rejectH)

	rollbackH := http.HandlerFunc(
		fleetRollbackActionHandler(deps.Fleet))
	route(mux, "POST /api/v1/actions/{id}/rollback", gateOperator, rollbackH)

	execH := http.HandlerFunc(
		fleetManualExecuteHandler(deps.Fleet))
	route(mux, "POST /api/v1/actions/execute", gateOperator, execH)
}

func registerUnavailableActionRoutes(mux *http.ServeMux) {
	notImpl := http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			jsonError(w,
				"action approval not available",
				http.StatusNotImplemented)
		})
	route(mux, "POST /api/v1/actions/{id}/approve", gateOperator, notImpl)
	route(mux, "POST /api/v1/actions/{id}/reject", gateOperator, notImpl)
	route(mux, "POST /api/v1/actions/{id}/rollback", gateOperator, notImpl)
	route(mux, "POST /api/v1/actions/execute", gateOperator, notImpl)
}
