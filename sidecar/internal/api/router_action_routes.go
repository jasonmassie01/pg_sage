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
	operatorUp := RequireRole("admin", "operator")
	registerPendingActionRoutes(mux, deps, operatorUp)

	switch {
	case deps.Store != nil && deps.Executor != nil:
		registerExecutorActionRoutes(mux, deps, operatorUp)
	case deps.Fleet != nil:
		registerFleetActionRoutes(mux, deps, operatorUp)
	default:
		registerUnavailableActionRoutes(mux, operatorUp)
	}
}

// registerPendingActionRoutes covers the pending queue and the per-finding
// lookup used by the inline action flow on the Findings page.
func registerPendingActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
	operatorUp func(http.Handler) http.Handler,
) {
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
}

func registerExecutorActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
	operatorUp func(http.Handler) http.Handler,
) {
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
}

func registerFleetActionRoutes(
	mux *http.ServeMux, deps *ActionDeps,
	operatorUp func(http.Handler) http.Handler,
) {
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
}

func registerUnavailableActionRoutes(
	mux *http.ServeMux, operatorUp func(http.Handler) http.Handler,
) {
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
