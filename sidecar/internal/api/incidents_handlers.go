package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/rca"
)

func incidentsListHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		dbName := q.Get("database")
		severity := q.Get("severity")
		status := q.Get("status")
		if err := validateDatabaseParam(dbName); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, dbName) {
			return
		}

		pools := poolsForDatabaseSelection(mgr, dbName)
		if len(pools) == 0 {
			jsonResponse(w, map[string]any{
				"incidents": []any{}, "total": 0,
			})
			return
		}

		var incidents []map[string]any
		for _, selected := range pools {
			dbIncidents, err := queryIncidents(
				r.Context(), selected.pool, "",
				severity, status)
			if err != nil {
				slog.Error("query incidents failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query incidents", 500)
				return
			}
			for _, incident := range dbIncidents {
				annotateIncidentFleetDatabase(incident, selected.name)
				incidents = append(incidents, incident)
			}
		}
		sortIncidentsByDetectedAt(incidents)
		if len(incidents) > 100 {
			incidents = incidents[:100]
		}
		if incidents == nil {
			incidents = []map[string]any{}
		}
		jsonResponse(w, map[string]any{
			"incidents": incidents, "total": len(incidents),
		})
	}
}

func incidentsActiveHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if rejectUnknownDatabase(w, mgr, dbName) {
			return
		}
		pools := poolsForDatabaseSelection(mgr, dbName)
		if len(pools) == 0 {
			jsonResponse(w, map[string]any{
				"incidents": []any{}, "total": 0,
			})
			return
		}

		var incidents []map[string]any
		for _, selected := range pools {
			dbIncidents, err := queryActiveIncidents(
				r.Context(), selected.pool, "")
			if err != nil {
				slog.Error("query active incidents failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query incidents", 500)
				return
			}
			for _, incident := range dbIncidents {
				annotateIncidentFleetDatabase(incident, selected.name)
				incidents = append(incidents, incident)
			}
		}
		sortIncidentsByDetectedAt(incidents)
		if len(incidents) > 100 {
			incidents = incidents[:100]
		}
		if incidents == nil {
			incidents = []map[string]any{}
		}
		jsonResponse(w, map[string]any{
			"incidents": incidents, "total": len(incidents),
		})
	}
}

func incidentDetailHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing incident id",
				http.StatusBadRequest)
			return
		}
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		selected, ok := resolveIncidentTargetPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required",
				http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "incident not found", http.StatusNotFound)
			return
		}
		incident, err := queryIncidentByID(
			r.Context(), selected.pool, id,
		)
		if err == nil {
			annotateIncidentFleetDatabase(incident, selected.name)
			jsonResponse(w, incident)
			return
		}
		jsonError(w, "incident not found", http.StatusNotFound)
	}
}

// maxResolutionReasonLen bounds the operator-supplied resolution reason.
const maxResolutionReasonLen = 2000

func incidentResolveHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing incident id",
				http.StatusBadRequest)
			return
		}
		reason, ok := readResolutionReason(w, r)
		if !ok {
			return
		}
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		selected, ok := resolveIncidentTargetPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required",
				http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "incident not found", http.StatusNotFound)
			return
		}
		err := rca.ResolveIncident(r.Context(), selected.pool, id,
			incidentResolver(r), reason)
		if !writeResolveError(w, err, selected.name, id) {
			return
		}
		jsonResponse(w, map[string]any{
			"ok":       true,
			"id":       id,
			"database": selected.name,
			"status":   "resolved",
		})
	}
}

// readResolutionReason decodes and bounds the resolve request body.
func readResolutionReason(
	w http.ResponseWriter, r *http.Request,
) (string, bool) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return "", false
	}
	reason := strings.TrimSpace(body.Reason)
	if len(reason) > maxResolutionReasonLen {
		jsonError(w, fmt.Sprintf("reason exceeds %d characters",
			maxResolutionReasonLen), http.StatusBadRequest)
		return "", false
	}
	return reason, true
}

// incidentResolver identifies who resolved an incident through the API.
func incidentResolver(r *http.Request) string {
	if u := UserFromContext(r.Context()); u != nil {
		if u.Email != "" {
			return "user:" + u.Email
		}
		return fmt.Sprintf("user:id=%d", u.ID)
	}
	return "api"
}

// writeResolveError maps rca.ResolveIncident errors to HTTP. It returns
// true when err is nil and the caller should write the success body.
func writeResolveError(
	w http.ResponseWriter, err error, database, id string,
) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, rca.ErrIncidentNotFound):
		jsonError(w, "incident not found", http.StatusNotFound)
	case errors.Is(err, rca.ErrIncidentAlreadyResolved):
		jsonError(w, "incident already resolved", http.StatusConflict)
	default:
		slog.Error("resolve incident failed",
			"database", database, "incident", id, "error", err)
		jsonError(w, "failed to resolve incident",
			http.StatusInternalServerError)
	}
	return false
}

func resolveIncidentTargetPool(
	mgr *fleet.DatabaseManager, dbName string,
) (namedPool, bool) {
	return resolveSingleDatabaseRequestPool(mgr, dbName)
}

func sortIncidentsByDetectedAt(incidents []map[string]any) {
	sort.SliceStable(incidents, func(i, j int) bool {
		return timeFromMap(incidents[i], "detected_at").After(
			timeFromMap(incidents[j], "detected_at"))
	})
}

func annotateIncidentFleetDatabase(incident map[string]any, alias string) {
	if incident == nil || alias == "" {
		return
	}
	incident["fleet_database_name"] = alias
	if fmt.Sprint(incident["database_name"]) == "" {
		incident["database_name"] = alias
	}
}
