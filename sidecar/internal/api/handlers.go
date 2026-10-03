package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/store"
)

func databasesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, mgr.FleetStatus())
	}
}

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

func queryFindingsStats(
	ctx context.Context, pool *pgxpool.Pool,
	f fleet.FindingFilters,
) ([]map[string]any, int, error) {
	where, args := buildFindingsWhere(f)
	query := `SELECT severity, category, COUNT(*) AS cnt
	 FROM sage.findings` + where +
		` GROUP BY severity, category
		  ORDER BY severity, category`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf(
			"query findings stats: %w", err)
	}
	defer rows.Close()

	var results []map[string]any
	total := 0
	for rows.Next() {
		var severity, category string
		var cnt int
		if err := rows.Scan(
			&severity, &category, &cnt,
		); err != nil {
			return nil, 0, fmt.Errorf(
				"scan findings stat: %w", err)
		}
		total += cnt
		results = append(results, map[string]any{
			"severity": severity,
			"category": category,
			"count":    cnt,
		})
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, total, nil
}

func queryFindingsStatsAcrossFleet(
	ctx context.Context, mgr *fleet.DatabaseManager,
	f fleet.FindingFilters,
) ([]map[string]any, int, error) {
	counts := make(map[string]int)
	total := 0
	for _, inst := range sortedFleetInstances(mgr) {
		if inst.Pool == nil {
			continue
		}
		stats, dbTotal, err := queryFindingsStats(
			ctx, inst.Pool, f,
		)
		if err != nil {
			return nil, 0, err
		}
		total += dbTotal
		for _, s := range stats {
			key := fmt.Sprintf("%s\x00%s",
				s["severity"], s["category"])
			if c, ok := s["count"].(int); ok {
				counts[key] += c
			}
		}
	}
	results := make([]map[string]any, 0, len(counts))
	for key, count := range counts {
		parts := strings.SplitN(key, "\x00", 2)
		results = append(results, map[string]any{
			"severity": parts[0],
			"category": parts[1],
			"count":    count,
		})
	}
	sort.Slice(results, func(i, j int) bool {
		si, sj := fmt.Sprint(results[i]["severity"]),
			fmt.Sprint(results[j]["severity"])
		if si != sj {
			return si < sj
		}
		return fmt.Sprint(results[i]["category"]) <
			fmt.Sprint(results[j]["category"])
	})
	if results == nil {
		results = []map[string]any{}
	}
	return results, total, nil
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

func actionsListHandler(
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
		page, err := parseListPage(q, actionsSort, "desc")
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := actionsRequest{page: page, from: parseTimeParam(q.Get("from")),
			to: parseTimeParam(q.Get("to"))}
		displayName := responseDatabaseName(database)
		if database == "all" && mgr.InstanceCount() == 1 {
			displayName = mgr.ResolveDatabaseName(database)
		}
		sources := poolsForDatabaseSelection(mgr, database)
		if len(sources) == 0 {
			jsonResponse(w, map[string]any{
				"database": displayName, "total": 0, "total_capped": false,
				"offset": page.Offset, "limit": page.Limit,
				"actions": []any{}, "next_cursor": "",
			})
			return
		}
		resp, err := listActions(r.Context(), sources, req)
		if err != nil {
			writeListError(w, r, "actions", err)
			return
		}
		resp["database"] = displayName
		jsonResponse(w, resp)
	}
}

func actionDetailHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing action id", http.StatusBadRequest)
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
			jsonError(w, "action not found", http.StatusNotFound)
			return
		}
		action, err := queryActionByID(
			r.Context(), selected.pool, id,
		)
		if err == nil {
			jsonResponse(w, action)
			return
		}
		jsonError(w, "action not found", http.StatusNotFound)
	}
}

func snapshotLatestHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, database)
		if !ok {
			if database == "all" {
				jsonResponse(w, map[string]any{
					"database": "all", "snapshot": nil,
				})
				return
			}
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		metric := r.URL.Query().Get("metric")
		if metric == "" {
			metric = "system"
		}
		if selected.pool == nil {
			if selected.name == "" {
				jsonError(w, "database not found", http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database": selected.name, "snapshot": nil,
			})
			return
		}
		displayName := selected.name
		data, err := querySnapshotLatest(
			r.Context(), selected.pool, metric,
		)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				internalError(w, r, "read latest snapshot", err)
				return
			}
			jsonResponse(w, map[string]any{
				"database": displayName, "snapshot": nil,
			})
			return
		}
		jsonResponse(w, map[string]any{
			"database": displayName, "snapshot": data,
		})
	}
}

func snapshotHistoryHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		metric := q.Get("metric")
		if database == "" {
			database = "all"
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, database)
		if !ok {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if msg := historyMetricProblem(metric); msg != "" {
			jsonError(w, msg, http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			if selected.name == "" {
				jsonError(w, "database not found", http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database": selected.name, "metric": metric,
				"points": []any{}, "truncated": false,
			})
			return
		}
		displayName := selected.name
		hours := parseIntDefault(q.Get("hours"), 24)
		from := parseTimeParam(q.Get("from"))
		to := parseTimeParam(q.Get("to"))
		points, truncated, err := querySnapshotHistory(
			r.Context(), selected.pool, metric, hours, from, to,
		)
		if err != nil {
			slog.Warn("snapshot history query failed; answering no points",
				"database", displayName, "metric", metric, "error", err)
			points, truncated = []historyPoint{}, false
		}
		jsonResponse(w, map[string]any{
			"database": displayName, "metric": metric,
			"points": points, "truncated": truncated,
		})
	}
}

// fleetHealthHandler returns a time-series of health scores per
// database from sage.health_history. Accepts `from`/`to` (RFC3339)
// or `hours` (default 24). When `database=` is set, results are
// filtered to that database; otherwise all databases are included.
// Response shape:
//
//	{
//	  "databases": {
//	    "db1": [{"t": "...", "health": 87, "critical": 1, ...}, ...],
//	    ...
//	  }
//	}
func fleetHealthHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		hours := parseIntDefault(q.Get("hours"), 24)
		from := parseTimeParam(q.Get("from"))
		to := parseTimeParam(q.Get("to"))
		if from.IsZero() && to.IsZero() {
			to = time.Now().UTC()
			from = to.Add(-time.Duration(hours) * time.Hour)
		} else {
			if from.IsZero() {
				from = time.Unix(0, 0)
			}
			if to.IsZero() {
				to = time.Now().UTC()
			}
		}

		out := map[string][]map[string]any{}
		// In fleet mode every instance has its own sage.health_history
		// written by RecordHealthSnapshots. We query each pool once.
		instances := mgr.Instances()
		for name, inst := range instances {
			if database != "" && database != "all" && database != name {
				continue
			}
			if inst.Pool == nil {
				continue
			}
			points, err := queryHealthHistory(
				r.Context(), inst.Pool, name, from, to,
			)
			if err != nil {
				slog.Warn("fleet_health: query failed",
					"database", name, "error", err)
				out[name] = []map[string]any{}
				continue
			}
			out[name] = points
		}
		jsonResponse(w, map[string]any{
			"from":      from,
			"to":        to,
			"databases": out,
		})
	}
}

func queryHealthHistory(
	ctx context.Context, pool *pgxpool.Pool,
	dbName string, from, to time.Time,
) ([]map[string]any, error) {
	rows, err := pool.Query(ctx,
		`/* pg_sage */ SELECT recorded_at, health_score, findings_open,
		        findings_critical, findings_warning,
		        findings_info, actions_total
		 FROM sage.health_history
		 WHERE database_name = $1
		 AND recorded_at BETWEEN $2 AND $3
		 ORDER BY recorded_at`,
		dbName, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query health_history: %w", err)
	}
	defer rows.Close()
	points := []map[string]any{}
	for rows.Next() {
		var (
			ts                                            time.Time
			score, open, critical, warning, info, actions int
		)
		if err := rows.Scan(
			&ts, &score, &open, &critical,
			&warning, &info, &actions,
		); err != nil {
			return nil, fmt.Errorf("scan health_history: %w", err)
		}
		points = append(points, map[string]any{
			"t":        ts,
			"health":   score,
			"open":     open,
			"critical": critical,
			"warning":  warning,
			"info":     info,
			"actions":  actions,
		})
	}
	return points, nil
}

func validateMetric(metric string) bool {
	if metric == "" {
		return true
	}
	valid := map[string]bool{
		// Collector snapshot categories.
		"tables": true, "indexes": true, "queries": true,
		"sequences": true, "foreign_keys": true, "system": true,
		"io": true, "locks": true, "config_data": true, "replication": true,
		"partitions": true,
		// Dashboard time-series metrics.
		"cache_hit_ratio": true, "connections": true,
		"tps": true, "dead_tuples": true,
		"database_size": true, "replication_lag": true,
	}
	return valid[metric]
}

func configGetHandler(
	mgr *fleet.DatabaseManager, cfg *config.Config,
	controllers ...*config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database != "" {
			inst := mgr.GetInstance(database)
			if inst == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database":    database,
				"trust_level": inst.Config.TrustLevel,
				"executor":    inst.Config.IsExecutorEnabled(),
				"llm":         inst.Config.IsLLMEnabled(),
				"tags":        inst.Config.Tags,
			})
			return
		}
		responseCfg := cfg
		response := make(map[string]any)
		if controller := firstConfigController(controllers); controller != nil {
			active := controller.Active()
			desired := controller.Desired()
			responseCfg = active.Config
			response["active_generation"] = active.Generation
			response["desired_generation"] = desired.Generation
		}
		response["mode"] = responseCfg.Mode
		response["trust"] = responseCfg.Trust
		response["collector"] = responseCfg.Collector
		response["analyzer"] = responseCfg.Analyzer
		response["safety"] = responseCfg.Safety
		response["llm_enabled"] = responseCfg.LLM.Enabled
		response["advisor"] = responseCfg.Advisor
		response["databases"] = len(responseCfg.Databases)
		jsonResponse(w, response)
	}
}

func configUpdateHandlerWithStore(
	mgr *fleet.DatabaseManager, cfg *config.Config,
	controller *config.ConfigController, cs *store.ConfigStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if controller != nil {
			expected, ok := configExpectedGeneration(body)
			if !ok {
				jsonError(w, "expected_generation is required",
					http.StatusPreconditionRequired)
				return
			}
			candidate := controller.Desired().Config
			if !applyTrustConfigUpdate(w, body, candidate) {
				return
			}
			result, err := applyConfigUpdateCandidate(
				r, controller, cs, expected, candidate,
			)
			if err != nil {
				if errors.Is(err, config.ErrGenerationConflict) {
					jsonError(w, err.Error(), http.StatusConflict)
					return
				}
				internalError(w, r, "apply config generation", err)
				return
			}
			jsonResponse(w, result)
			return
		}
		if !applyTrustConfigUpdate(w, body, cfg) {
			return
		}
		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func applyConfigUpdateCandidate(
	r *http.Request, controller *config.ConfigController,
	cs *store.ConfigStore, expected uint64, candidate *config.Config,
) (config.ApplyResult, error) {
	if cs == nil {
		return controller.Apply(r.Context(), expected, candidate)
	}
	userID := 0
	if user := UserFromContext(r.Context()); user != nil {
		userID = user.ID
	}
	writes := []store.ConfigOverrideWrite{{
		Key: "trust.level", Value: candidate.Trust.Level,
	}}
	return controller.ApplyWithPersistence(
		r.Context(), expected, candidate,
		func(ctx context.Context, snapshot config.ConfigSnapshot) error {
			generation, err := cs.SetOverridesCAS(
				ctx, writes, 0, userID, expected,
			)
			if err == nil && generation != snapshot.Generation {
				return fmt.Errorf("durable generation %d, controller %d",
					generation, snapshot.Generation)
			}
			return err
		},
	)
}

func applyTrustConfigUpdate(
	w http.ResponseWriter, body map[string]any, candidate *config.Config,
) bool {
	if trust, ok := body["trust"]; ok {
		if trustMap, ok := trust.(map[string]any); ok {
			if level, ok := trustMap["level"].(string); ok {
				valid := map[string]bool{
					"observation": true, "advisory": true,
					"autonomous": true,
				}
				if !valid[level] {
					jsonError(w, "invalid trust level",
						http.StatusBadRequest)
					return false
				}
				candidate.Trust.Level = level
			}
		}
	}
	return true
}

func firstConfigController(
	controllers []*config.ConfigController,
) *config.ConfigController {
	if len(controllers) == 0 {
		return nil
	}
	return controllers[0]
}

func configExpectedGeneration(body map[string]any) (uint64, bool) {
	raw, ok := body["expected_generation"]
	if !ok {
		return 0, false
	}
	value, ok := raw.(float64)
	if !ok || value < 1 || value != float64(uint64(value)) {
		return 0, false
	}
	return uint64(value), true
}

func metricsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		status := mgr.FleetStatus()
		if database != "" {
			inst := mgr.GetInstance(database)
			if inst == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database": database,
				"status":   inst.Status,
			})
			return
		}
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
		stopped, err := mgr.EmergencyStopStrict(database, authenticatedActor(r))
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
			"stopped": stopped, "status": "stopped",
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
			jsonError(w, "failed to persist emergency stop",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"resumed": resumed, "status": "resumed",
		})
	}
}

// --- Query helpers (extracted to keep handlers under 50 lines) ---

func parseFindingFilters(q map[string][]string) fleet.FindingFilters {
	f := fleet.FindingFilters{
		Status:           valOrDefault(q, "status", "open"),
		Severity:         valOrDefault(q, "severity", ""),
		Category:         valOrDefault(q, "category", ""),
		Source:           valOrDefault(q, "source", ""),
		ThematicCategory: valOrDefault(q, "thematic_category", ""),
		Sort:             valOrDefault(q, "sort", "severity"),
		Order:            valOrDefault(q, "order", "desc"),
		Limit:            parseIntDefault(firstVal(q, "limit"), 50),
		Offset:           parseIntDefault(firstVal(q, "offset"), 0),
		From:             parseTimeParam(firstVal(q, "from")),
		To:               parseTimeParam(firstVal(q, "to")),
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	return f
}

// parseTimeParam parses an ISO-8601/RFC-3339 timestamp, returning zero
// time on parse failure so callers can treat it as "unset".
func parseTimeParam(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func valOrDefault(
	q map[string][]string, key, def string,
) string {
	if vals, ok := q[key]; ok && len(vals) > 0 && vals[0] != "" {
		return vals[0]
	}
	return def
}

func firstVal(q map[string][]string, key string) string {
	if vals, ok := q[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func findingsEmptyResponse(
	database string, filters fleet.FindingFilters,
) map[string]any {
	return map[string]any{
		"database":     database,
		"filters":      filters,
		"total":        0,
		"total_capped": false,
		"offset":       filters.Offset,
		"limit":        filters.Limit,
		"findings":     []any{},
		"next_cursor":  "",
	}
}

func sortedFleetInstances(
	mgr *fleet.DatabaseManager,
) []*fleet.DatabaseInstance {
	instances := mgr.Instances()
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*fleet.DatabaseInstance, 0, len(names))
	for _, name := range names {
		out = append(out, instances[name])
	}
	return out
}

func buildFindingsWhere(
	f fleet.FindingFilters,
) (string, []any) {
	where := " WHERE 1=1"
	var args []any
	n := 1
	where += selfmonitor.FindingsSQLExclusionClause()
	if f.Status != "" {
		where += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, f.Status)
		n++
	}
	if f.Severity != "" {
		where += fmt.Sprintf(" AND severity = $%d", n)
		args = append(args, f.Severity)
		n++
	}
	if f.Category != "" {
		where += fmt.Sprintf(" AND category = $%d", n)
		args = append(args, f.Category)
		n++
	}
	// Source filter — subsystem taxonomy mapping (see §16 of
	// docs/ui-redesign-v2.md). The mapping is an allowlist-to-SQL
	// rewrite. Unknown source values are treated as no-filter so the
	// UI can add new subsystems ahead of backend awareness without
	// dropping the page.
	if clause, a, ok := buildSourceClause(f.Source, n); ok {
		where += clause
		args = append(args, a...)
		n += len(a)
	}
	// ThematicCategory only applies when rows are lint rows, whose
	// detail JSONB carries thematic_category ('indexing',
	// 'performance', etc.). For non-lint queries the filter is
	// effectively a no-op since their detail lacks that key.
	if f.ThematicCategory != "" {
		where += fmt.Sprintf(
			" AND detail->>'thematic_category' = $%d", n)
		args = append(args, f.ThematicCategory)
		n++
	}
	// Overlapping-window time filter (findings live across time):
	// include a finding when it was open at any point in [From, To].
	if !f.From.IsZero() {
		where += fmt.Sprintf(
			" AND (resolved_at IS NULL OR resolved_at >= $%d)", n)
		args = append(args, f.From)
		n++
	}
	if !f.To.IsZero() {
		where += fmt.Sprintf(" AND created_at <= $%d", n)
		args = append(args, f.To)
	}
	return where, args
}

// buildSourceClause converts a subsystem filter value into a WHERE
// fragment + args. Returns ok=false when the value is empty or
// unrecognised (caller must then emit nothing).
func buildSourceClause(
	source string, startN int,
) (string, []any, bool) {
	if source == "" {
		return "", nil, false
	}
	switch source {
	case "schema_lint":
		return fmt.Sprintf(" AND category LIKE $%d", startN),
			[]any{"schema_lint:%"}, true
	case "rules":
		return fmt.Sprintf(
				" AND category = ANY($%d::text[])", startN),
			[]any{rulesCategories}, true
	case "forecaster":
		return fmt.Sprintf(" AND category LIKE $%d", startN),
			[]any{"forecast%"}, true
	case "query_tuning":
		return fmt.Sprintf(
				" AND category = ANY($%d::text[])", startN),
			[]any{queryTuningCategories}, true
	case "advisor":
		return fmt.Sprintf(
				" AND (category = 'advisor' OR "+
					"detail->>'subsystem' = $%d)", startN),
			[]any{"advisor"}, true
	case "optimizer":
		return fmt.Sprintf(
				" AND (category = 'optimizer' OR "+
					"detail->>'subsystem' = $%d)", startN),
			[]any{"optimizer"}, true
	case "incident":
		return fmt.Sprintf(
				" AND category = ANY($%d::text[])", startN),
			[]any{incidentCategories}, true
	case "migration_advisor":
		return " AND 1=0", nil, true // reserved, emits nothing
	}
	return "", nil, false
}

var (
	// rulesCategories is the analyzer Tier-1 allowlist.
	rulesCategories = []string{
		"index_health", "unused_index", "invalid_index",
		"duplicate_index", "missing_fk_index", "slow_query",
		"high_plan_time", "query_regression", "seq_scan_heavy",
		"high_total_time", "lock_chain", "connection_leak",
		"cache_hit_ratio", "checkpoint_pressure",
		"stat_statements_pressure", "replication_lag",
		"inactive_slot", "slow_replication_slot",
		"sequence_exhaustion", "sort_without_index",
		"work_mem_promotion", "table_bloat", "xid_wraparound",
		"extension_drift", "plan_regression",
	}
	queryTuningCategories = []string{
		"query_tuning", "stale_statistics", "runaway_query",
	}
	incidentCategories = []string{
		"incident", "incident_open", "incident_resolved",
	}
)

func buildFindingMapWithAction(
	id int64, createdAt, lastSeen time.Time,
	occurrenceCount int, category, severity string,
	objectType, objectIdent *string, title string,
	detail []byte, recommendation, recommendedSQL, rollbackSQL *string,
	status, database string, ruleID *string,
	impactScore *float64, resolvedAt, actedOnAt *time.Time,
	actionLogID *int64,
) map[string]any {
	// detail is jsonb in the DB, but a corrupt row or pre-jsonb
	// legacy bytes can land here as invalid JSON. Surface the raw
	// string on decode failure so operators still see the data,
	// and log once so the malformed row is traceable.
	var detailParsed any
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &detailParsed); err != nil {
			slog.Warn(
				"findings: detail column is not valid JSON",
				"finding_id", id,
				"err", err,
			)
			detailParsed = string(detail)
		}
	}
	return map[string]any{
		"id":                strconv.FormatInt(id, 10),
		"created_at":        createdAt,
		"last_seen":         lastSeen,
		"occurrence_count":  occurrenceCount,
		"category":          category,
		"severity":          severity,
		"object_type":       derefStr(objectType),
		"object_identifier": derefStr(objectIdent),
		"title":             title,
		"detail":            detailParsed,
		"recommendation":    derefStr(recommendation),
		"recommended_sql":   derefStr(recommendedSQL),
		"rollback_sql":      derefStr(rollbackSQL),
		"status":            status,
		"database_name":     database,
		"rule_id":           derefStr(ruleID),
		"impact_score":      impactScore,
		"resolved_at":       resolvedAt,
		"acted_on_at":       actedOnAt,
		"action_log_id":     actionLogID,
		"subsystem":         subsystemFromCategory(category),
	}
}

// subsystemFromCategory maps a category value to the subsystem the
// UI shows in the source filter. Kept in lockstep with §16 appendix.
func subsystemFromCategory(category string) string {
	if strings.HasPrefix(category, "schema_lint:") {
		return "schema_lint"
	}
	if strings.HasPrefix(category, "forecast") {
		return "forecaster"
	}
	for _, c := range queryTuningCategories {
		if c == category {
			return "query_tuning"
		}
	}
	for _, c := range incidentCategories {
		if c == category {
			return "incident"
		}
	}
	for _, c := range rulesCategories {
		if c == category {
			return "rules"
		}
	}
	if category == "advisor" {
		return "advisor"
	}
	if category == "optimizer" {
		return "optimizer"
	}
	return ""
}

func queryFindingByID(
	ctx context.Context, pool *pgxpool.Pool, id string,
) (map[string]any, error) {
	var (
		fID              int64
		createdAt        time.Time
		lastSeen         time.Time
		occurrenceCount  int
		category         string
		severity         string
		objectType       *string
		objectIdent      *string
		title            string
		detail           []byte
		recommendation   *string
		recommendedSQL   *string
		rollbackSQL      *string
		estimatedCostUSD *float64
		status           string
		suppressedUntil  *time.Time
		resolvedAt       *time.Time
		actedOnAt        *time.Time
		actionLogID      *int64
	)
	err := pool.QueryRow(ctx, findingDetailSQL, id).Scan(
		&fID, &createdAt, &lastSeen, &occurrenceCount,
		&category, &severity, &objectType, &objectIdent,
		&title, &detail, &recommendation, &recommendedSQL,
		&rollbackSQL, &estimatedCostUSD, &status,
		&suppressedUntil, &resolvedAt, &actedOnAt,
		&actionLogID,
	)
	if err != nil {
		return nil, err
	}
	// See getFindings — bad JSON returns the raw string rather than
	// masquerading as a missing field, with a log entry for audit.
	var detailParsed any
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &detailParsed); err != nil {
			slog.Warn(
				"finding_detail: detail column is not valid JSON",
				"finding_id", fID,
				"err", err,
			)
			detailParsed = string(detail)
		}
	}
	return map[string]any{
		"id":                 strconv.FormatInt(fID, 10),
		"created_at":         createdAt,
		"last_seen":          lastSeen,
		"occurrence_count":   occurrenceCount,
		"category":           category,
		"severity":           severity,
		"object_type":        derefStr(objectType),
		"object_identifier":  derefStr(objectIdent),
		"title":              title,
		"detail":             detailParsed,
		"recommendation":     derefStr(recommendation),
		"recommended_sql":    derefStr(recommendedSQL),
		"rollback_sql":       derefStr(rollbackSQL),
		"estimated_cost_usd": estimatedCostUSD,
		"status":             status,
		"suppressed_until":   suppressedUntil,
		"resolved_at":        resolvedAt,
		"acted_on_at":        actedOnAt,
		"action_log_id":      actionLogID,
	}, nil
}

const findingDetailSQL = `/* pg_sage */SELECT id, created_at, last_seen,
 occurrence_count, category, severity, object_type,
 object_identifier, title, detail, recommendation,
 recommended_sql, rollback_sql, estimated_cost_usd,
 status, suppressed_until, resolved_at, acted_on_at,
 action_log_id
 FROM sage.findings WHERE id = $1`

func updateFindingStatus(
	ctx context.Context, pool *pgxpool.Pool,
	id, fromStatus, toStatus string,
) error {
	tag, err := pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.findings SET status = $1
		 WHERE id = $2 AND status = $3`,
		toStatus, id, fromStatus,
	)
	if err != nil {
		// Unique constraint on (category, object_identifier)
		// for open findings — an open finding already exists.
		if strings.Contains(err.Error(), "idx_findings_dedup") {
			// Delete the stale suppressed finding instead of
			// unsuppressing it since there's already an active
			// open finding for the same issue.
			_, delErr := pool.Exec(ctx,
				`/* pg_sage */ DELETE FROM sage.findings
				 WHERE id = $1 AND status = 'suppressed'`, id)
			if delErr != nil {
				return fmt.Errorf(
					"conflict cleanup failed: %w", delErr)
			}
			return nil
		}
		return fmt.Errorf("update finding status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no matching finding")
	}
	return nil
}

// isConnectionError returns true if the error indicates a
// database connectivity problem rather than a query-level issue.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	markers := []string{
		"closed pool",
		"connection refused",
		"connection reset",
		"broken pipe",
		"no such host",
		"i/o timeout",
		"context deadline exceeded",
		"connection timed out",
		"pool closed",
	}
	lower := strings.ToLower(msg)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func queryActionByID(
	ctx context.Context, pool *pgxpool.Pool, id string,
) (map[string]any, error) {
	var (
		aID            int64
		executedAt     time.Time
		actionType     string
		findingID      *int64
		sqlExecuted    string
		rollbackSQL    *string
		beforeState    []byte
		afterState     []byte
		outcome        string
		rollbackReason *string
		measuredAt     *time.Time
	)
	err := pool.QueryRow(ctx, actionDetailSQL, id).Scan(
		&aID, &executedAt, &actionType, &findingID,
		&sqlExecuted, &rollbackSQL, &beforeState,
		&afterState, &outcome, &rollbackReason, &measuredAt,
	)
	if err != nil {
		return nil, err
	}
	return buildActionMap(
		aID, executedAt, actionType, findingID,
		sqlExecuted, rollbackSQL, beforeState, afterState,
		outcome, rollbackReason, measuredAt,
	), nil
}

const actionDetailSQL = `/* pg_sage */SELECT id, executed_at,
 action_type, finding_id, sql_executed, rollback_sql,
 before_state, after_state, outcome, rollback_reason,
 measured_at FROM sage.action_log WHERE id = $1`

const snapshotHistoryMaxPoints = 500

func querySnapshotLatest(
	ctx context.Context, pool *pgxpool.Pool, metric string,
) (any, error) {
	var data []byte
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT `+snapstore.DataSQL("")+` FROM sage.snapshots
		 WHERE category = $1
		 ORDER BY collected_at DESC LIMIT 1`,
		metric,
	).Scan(&data)
	if err != nil {
		return nil, err
	}
	var parsed any
	_ = json.Unmarshal(data, &parsed)
	return parsed, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
