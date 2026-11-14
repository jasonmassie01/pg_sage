package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// registerWorkloadRoutes registers the workload explorer's viewer routes
// (market-01; handlers in queries_handlers.go, query_detail.go and
// wait_load.go).
func registerWorkloadRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET /api/v1/queries/top", gateViewer, mgr,
		aggregateHandler(queriesTopHandler))
	routeAggregate(mux, "GET /api/v1/queries/{queryid}", gateViewer, mgr,
		aggregateHandler(queryDetailHandler))
	routeAggregate(mux, "GET /api/v1/wait-load", gateViewer, mgr, aggregateHandler(waitLoadHandler))
}
