package api

import (
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func metricsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database != "" {
			row, found := mgr.InstanceStatus(database)
			if !found {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database": database,
				"status":   row.Status,
			})
			return
		}
		status := mgr.FleetStatus()
		jsonResponse(w, map[string]any{
			"fleet":     status.Summary,
			"databases": status.Databases,
		})
	}
}

func emergencyStopHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		result, err := mgr.EmergencyStopDetailed(database, authenticatedActor(r))
		if err != nil {
			if errors.Is(err, fleet.ErrDatabaseNotFound) {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			jsonError(w, "failed to persist emergency stop",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"stopped": result.Changed, "status": "stopped",
			"cancelled_actions": result.Cancelled,
			"cancel_failures":   result.CancelFailures,
		})
	}
}

func resumeHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		resumed, err := mgr.ResumeStrict(database, authenticatedActor(r))
		if err != nil {
			if errors.Is(err, fleet.ErrDatabaseNotFound) {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			if errors.Is(err, fleet.ErrFleetStopped) {
				jsonError(w, fleet.ErrFleetStopped.Error(), http.StatusConflict)
				return
			}
			jsonError(w, "failed to persist emergency stop",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"resumed": resumed, "status": "resumed",
		})
	}
}
