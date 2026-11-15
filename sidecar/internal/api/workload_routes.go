package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// registerWorkloadRoutes registers the workload explorer's viewer routes
// (market-01; handlers in queries_handlers.go, query_detail.go and
// wait_load.go; evid-01b's coverage in queries_coverage.go). waitLoad adds Lakebase wait counters to the wait load
// (market-01b); nil leaves the route as it was. Each route reads through
// the caller's visible databases (routeAggregate).
func registerWorkloadRoutes(
	mux *http.ServeMux, mgr *fleet.DatabaseManager, waitLoad LakebaseWaitLoadFunc,
) {
	routeAggregate(mux, "GET /api/v1/queries/top", gateViewer, mgr,
		aggregateHandler(queriesTopHandler))
	routeAggregate(mux, "GET /api/v1/queries/coverage", gateViewer, mgr,
		aggregateHandler(queriesCoverageHandler))
	routeAggregate(mux, "GET /api/v1/queries/{queryid}", gateViewer, mgr,
		aggregateHandler(queryDetailHandler))
	routeAggregate(mux, "GET /api/v1/wait-load", gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler { return waitLoadHandler(m, waitLoad) })
}
