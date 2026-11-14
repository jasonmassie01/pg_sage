package api

import (
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func findingsListHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		filters := parseFindingFilters(q)
		page, err := parseListPage(q, filters.Sort, filters.Order)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		filters.Limit, filters.Offset = page.Limit, page.Offset
		displayName := mgr.ResolveDatabaseName(database)
		if database == "all" && mgr.InstanceCount() > 1 {
			displayName = "all"
		}
		sources := listSources(mgr, database)
		if len(sources) == 0 {
			jsonResponse(w, findingsEmptyResponse(displayName, filters))
			return
		}
		resp, err := listFindings(r.Context(), sources, filters, page)
		if err != nil {
			writeListError(w, r, "findings", err)
			return
		}
		resp["database"] = displayName
		resp["cluster_refs"] = clusterRefs(r.Context(), mgr, database)
		jsonResponse(w, resp)
	}
}

// findingsStatsHandler returns per-(severity,category) counts of
// findings matching the supplied filters. Used by the schema-health
// UI summary card. Accepts the same filters as findingsListHandler
// (status, severity, source, thematic_category, database).
func findingsStatsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		filters := parseFindingFilters(q)
		if database == "all" && mgr.InstanceCount() > 1 {
			stats, total, err := queryFindingsStatsAcrossFleet(
				r.Context(), mgr, filters,
			)
			if err != nil {
				slog.Error("query fleet findings stats failed",
					"error", err)
				jsonError(w, "failed to query findings stats", 500)
				return
			}
			jsonResponse(w, map[string]any{
				"database":   "all",
				"stats":      stats,
				"total_open": total,
			})
			return
		}
		pool := mgr.PoolForDatabase(database)
		displayName := mgr.ResolveDatabaseName(database)
		if pool == nil {
			jsonResponse(w, map[string]any{
				"database":   displayName,
				"stats":      []any{},
				"total_open": 0,
			})
			return
		}
		stats, total, err := queryFindingsStats(
			r.Context(), pool, filters,
		)
		if err != nil {
			slog.Error("query findings stats failed",
				"error", err)
			jsonError(w, "failed to query findings stats", 500)
			return
		}
		jsonResponse(w, map[string]any{
			"database":   displayName,
			"stats":      stats,
			"total_open": total,
		})
	}
}

func findingDetailHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing finding id", http.StatusBadRequest)
			return
		}
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required",
				http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "finding not found", http.StatusNotFound)
			return
		}
		finding, err := queryFindingByID(
			r.Context(), selected.pool, id,
		)
		if err == nil {
			jsonResponse(w, finding)
			return
		}
		jsonError(w, "finding not found", http.StatusNotFound)
	}
}

func suppressHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing finding id",
				http.StatusBadRequest)
			return
		}
		if dbName := r.URL.Query().Get("database"); dbName != "" &&
			dbName != "all" {
			pool := mgr.PoolForDatabase(dbName)
			if pool == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			err := updateFindingStatus(
				r.Context(), pool, id, "open", "suppressed",
			)
			if err != nil {
				jsonError(w, "finding not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"ok": true, "id": id, "status": "suppressed",
			})
			return
		}
		if !allowImplicitSingleDatabaseMutation(w, mgr) {
			return
		}
		pools := mgr.AllPools()
		if len(pools) == 0 {
			jsonResponse(w, map[string]string{
				"id": id, "status": "suppressed",
			})
			return
		}
		connErrors := 0
		for _, pool := range pools {
			err := updateFindingStatus(
				r.Context(), pool, id, "open", "suppressed",
			)
			if err == nil {
				jsonResponse(w, map[string]any{
					"ok": true, "id": id, "status": "suppressed",
				})
				return
			}
			if isConnectionError(err) {
				connErrors++
			}
		}
		if connErrors > 0 {
			jsonError(w,
				"database connection error — "+
					"some databases are unreachable",
				http.StatusServiceUnavailable)
			return
		}
		jsonError(w, "finding not found",
			http.StatusNotFound)
	}
}

func unsuppressHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing finding id",
				http.StatusBadRequest)
			return
		}
		if dbName := r.URL.Query().Get("database"); dbName != "" &&
			dbName != "all" {
			pool := mgr.PoolForDatabase(dbName)
			if pool == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			err := updateFindingStatus(
				r.Context(), pool, id, "suppressed", "open",
			)
			if err != nil {
				jsonError(w, "finding not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"ok": true, "id": id, "status": "open",
			})
			return
		}
		if !allowImplicitSingleDatabaseMutation(w, mgr) {
			return
		}
		pools := mgr.AllPools()
		if len(pools) == 0 {
			jsonResponse(w, map[string]string{
				"id": id, "status": "open",
			})
			return
		}
		connErrors := 0
		for _, pool := range pools {
			err := updateFindingStatus(
				r.Context(), pool, id, "suppressed", "open",
			)
			if err == nil {
				jsonResponse(w, map[string]any{
					"ok": true, "id": id, "status": "open",
				})
				return
			}
			if isConnectionError(err) {
				connErrors++
			}
		}
		if connErrors > 0 {
			jsonError(w,
				"database connection error — "+
					"some databases are unreachable",
				http.StatusServiceUnavailable)
			return
		}
		jsonError(w, "finding not found",
			http.StatusNotFound)
	}
}

func allowImplicitSingleDatabaseMutation(
	w http.ResponseWriter,
	mgr *fleet.DatabaseManager,
) bool {
	instances := mgr.Instances()
	if len(instances) != 1 {
		jsonError(w, "database is required",
			http.StatusBadRequest)
		return false
	}
	for _, inst := range instances {
		if inst.Pool == nil {
			jsonError(w, "database not found",
				http.StatusNotFound)
			return false
		}
		return true
	}
	jsonError(w, "database is required", http.StatusBadRequest)
	return false
}
