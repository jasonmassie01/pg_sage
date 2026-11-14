package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// registerWorkloadRoutes registers the workload explorer's viewer routes
// (market-01; handlers in queries_handlers.go, query_detail.go and
// wait_load.go). waitLoad adds Lakebase wait counters to the wait load
// (market-01b); nil leaves the route as it was.
func registerWorkloadRoutes(
	mux *http.ServeMux, mgr *fleet.DatabaseManager, waitLoad LakebaseWaitLoadFunc,
) {
	route(mux, "GET /api/v1/queries/top", gateViewer, http.HandlerFunc(queriesTopHandler(mgr)))
	route(mux, "GET /api/v1/queries/{queryid}", gateViewer,
		http.HandlerFunc(queryDetailHandler(mgr)))
	route(mux, "GET /api/v1/wait-load", gateViewer,
		http.HandlerFunc(waitLoadHandler(mgr, waitLoad)))
}
