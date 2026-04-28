package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func fleetReadinessHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if mgr == nil {
			jsonResponse(w, fleet.BuildFleetReadiness("", nil))
			return
		}
		overview := mgr.FleetStatus()
		jsonResponse(w, fleet.BuildFleetReadiness(
			overview.Mode, overview.Databases))
	}
}
