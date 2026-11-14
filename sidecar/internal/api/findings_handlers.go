package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

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
			writeFleetFindingsStats(w, r, mgr, filters)
			return
		}
		writeDatabaseFindingsStats(w, r, mgr, database, filters)
	}
}

// writeFleetFindingsStats answers the stats summed over every database.
func writeFleetFindingsStats(
	w http.ResponseWriter, r *http.Request,
	mgr *fleet.DatabaseManager, filters fleet.FindingFilters,
) {
	stats, total, err := queryFindingsStatsAcrossFleet(
		r.Context(), mgr, filters,
	)
	if err != nil {
		slog.Error("query fleet findings stats failed",
			"error", err)
		jsonError(w, "failed to query findings stats", 500)
		return
	}
	jsonResponse(w, findingsStatsResponse("all", stats, total))
}

// writeDatabaseFindingsStats answers one database's stats; a database
// without a pool has none.
func writeDatabaseFindingsStats(
	w http.ResponseWriter, r *http.Request,
	mgr *fleet.DatabaseManager, database string, filters fleet.FindingFilters,
) {
	pool := mgr.PoolForDatabase(database)
	displayName := mgr.ResolveDatabaseName(database)
	if pool == nil {
		jsonResponse(w, findingsStatsResponse(displayName, []any{}, 0))
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
	jsonResponse(w, findingsStatsResponse(displayName, stats, total))
}

func findingsStatsResponse(database string, stats any, total int) map[string]any {
	return map[string]any{
		"database":   database,
		"stats":      stats,
		"total_open": total,
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
	return findingStatusHandler(mgr, "open", "suppressed")
}

func unsuppressHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return findingStatusHandler(mgr, "suppressed", "open")
}

// findingStatusHandler moves one finding from status from to status to:
// on the named database, or on the only database when none is named.
func findingStatusHandler(
	mgr *fleet.DatabaseManager, from, to string,
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
			setNamedFindingStatus(w, r, mgr.PoolForDatabase(dbName), id, from, to)
			return
		}
		if !allowImplicitSingleDatabaseMutation(w, mgr) {
			return
		}
		setImplicitFindingStatus(w, r, mgr.AllPools(), id, from, to)
	}
}

func setNamedFindingStatus(
	w http.ResponseWriter, r *http.Request,
	pool *pgxpool.Pool, id, from, to string,
) {
	if pool == nil {
		jsonError(w, "database not found",
			http.StatusNotFound)
		return
	}
	err := updateFindingStatus(
		r.Context(), pool, id, from, to,
	)
	if err != nil {
		jsonError(w, "finding not found",
			http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]any{
		"ok": true, "id": id, "status": to,
	})
}

// setImplicitFindingStatus tries each pool until one holds the finding;
// an unreachable pool makes a miss a 503 rather than a 404.
func setImplicitFindingStatus(
	w http.ResponseWriter, r *http.Request,
	pools []*pgxpool.Pool, id, from, to string,
) {
	if len(pools) == 0 {
		jsonResponse(w, map[string]string{
			"id": id, "status": to,
		})
		return
	}
	connErrors := 0
	for _, pool := range pools {
		err := updateFindingStatus(
			r.Context(), pool, id, from, to,
		)
		if err == nil {
			jsonResponse(w, map[string]any{
				"ok": true, "id": id, "status": to,
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
