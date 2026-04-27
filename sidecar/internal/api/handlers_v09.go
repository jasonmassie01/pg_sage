package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// ---------- Incidents ----------

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
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid request body",
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

		err := resolveIncident(
			r.Context(), selected.pool, id, body.Reason,
		)
		if err != nil {
			jsonError(w, "incident not found", http.StatusNotFound)
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

// ---------- Explain ----------

func explainHandler(
	mgr *fleet.DatabaseManager, cfg *config.Config,
	llmClient *llm.Client,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.Explain.Enabled {
			jsonError(w, "explain endpoint is disabled",
				http.StatusServiceUnavailable)
			return
		}

		var req explain.ExplainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}

		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		pool := mgr.PoolForDatabase(dbName)
		if pool == nil {
			pool = mgr.PoolForDatabase("all")
		}
		if pool == nil {
			jsonError(w, "no database pool available",
				http.StatusServiceUnavailable)
			return
		}

		logFn := func(level, msg string, args ...any) {
			slog.Log(r.Context(), slog.LevelInfo, msg, args...)
		}
		ex := explain.NewWithLLM(pool, &cfg.Explain, llmClient, logFn)
		result, err := ex.Explain(r.Context(), req)
		if err != nil {
			if errors.Is(err, explain.ErrExplainInvalidRequest) {
				jsonError(w, err.Error(),
					http.StatusBadRequest)
				return
			}
			internalError(w, r, "explain query", err)
			return
		}
		jsonResponse(w, result)
	}
}

// ---------- Growth Forecasts ----------

func growthForecastHandler(
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
		displayName := responseDatabaseName(database)
		pools := poolsForDatabaseSelection(mgr, database)
		if len(pools) == 0 {
			jsonResponse(w, map[string]any{
				"database":  displayName,
				"forecasts": []any{},
			})
			return
		}
		days := parseIntDefault(q.Get("days"), 90)
		if days > 365 {
			days = 365
		}

		var forecasts []map[string]any
		for _, selected := range pools {
			dbForecasts, err := queryGrowthForecasts(
				r.Context(), selected.pool, days)
			if err != nil {
				slog.Error("query growth forecasts failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query growth forecasts",
					500)
				return
			}
			for _, forecast := range dbForecasts {
				forecast["fleet_database_name"] = selected.name
				if forecast["database_name"] == "" {
					forecast["database_name"] = selected.name
				}
				forecasts = append(forecasts, forecast)
			}
		}
		if forecasts == nil {
			forecasts = []map[string]any{}
		}
		jsonResponse(w, map[string]any{
			"database":  displayName,
			"forecasts": forecasts,
		})
	}
}

// ---------- Query helpers ----------

func resolveIncidentPool(
	mgr *fleet.DatabaseManager, dbName string,
) *pgxpool.Pool {
	if dbName != "" {
		pool := mgr.PoolForDatabase(dbName)
		if pool != nil {
			return pool
		}
		return nil
	}
	return mgr.PoolForDatabase("all")
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

const incidentsBaseSQL = `SELECT id, detected_at,
 COALESCE(last_detected_at, detected_at) AS last_detected_at,
 severity, root_cause, causal_chain, affected_objects, signal_ids,
 recommended_sql, action_risk, source, confidence,
 resolved_at, database_name, occurrence_count, escalated_at
 FROM sage.incidents`

func queryIncidents(
	ctx context.Context, pool *pgxpool.Pool,
	dbName, severity, status string,
) ([]map[string]any, error) {
	where := " WHERE 1=1"
	var args []any
	argN := 1

	if status == "resolved" {
		where += " AND resolved_at IS NOT NULL"
	} else {
		// Default to open (unresolved) incidents.
		where += " AND resolved_at IS NULL"
	}
	if severity != "" {
		where += fmt.Sprintf(" AND severity = $%d", argN)
		args = append(args, severity)
		argN++
	}
	if dbName != "" {
		where += fmt.Sprintf(" AND database_name = $%d", argN)
		args = append(args, dbName)
		argN++
	}

	query := incidentsBaseSQL + where +
		" ORDER BY detected_at DESC LIMIT 100"
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query incidents: %w", err)
	}
	defer rows.Close()
	return scanIncidentRows(rows)
}

func queryActiveIncidents(
	ctx context.Context, pool *pgxpool.Pool, dbName string,
) ([]map[string]any, error) {
	where := " WHERE resolved_at IS NULL"
	var args []any
	if dbName != "" {
		where += " AND database_name = $1"
		args = append(args, dbName)
	}
	query := incidentsBaseSQL + where +
		` ORDER BY CASE severity
		    WHEN 'critical' THEN 1
		    WHEN 'warning' THEN 2
		    ELSE 3 END, detected_at DESC LIMIT 100`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query active incidents: %w", err)
	}
	defer rows.Close()
	return scanIncidentRows(rows)
}

func queryIncidentByID(
	ctx context.Context, pool *pgxpool.Pool, id string,
) (map[string]any, error) {
	query := incidentsBaseSQL +
		` WHERE id = $1`
	row := pool.QueryRow(ctx, query, id)
	return scanIncidentRow(row)
}

func resolveIncident(
	ctx context.Context, pool *pgxpool.Pool,
	id, reason string,
) error {
	tag, err := pool.Exec(ctx,
		`UPDATE sage.incidents
		 SET resolved_at = now()
		 WHERE id = $1 AND resolved_at IS NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("resolve incident: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("incident not found or already resolved")
	}
	_ = reason // logged for audit; future: store in resolution_log
	return nil
}

type incidentRow struct {
	ID              string
	DetectedAt      time.Time
	LastDetectedAt  time.Time
	Severity        string
	RootCause       string
	CausalChain     []byte
	AffectedObjects []string
	SignalIDs       []string
	RecommendedSQL  *string
	ActionRisk      *string
	Source          string
	Confidence      float64
	ResolvedAt      *time.Time
	DatabaseName    *string
	OccurrenceCount int
	EscalatedAt     *time.Time
}

func (ir *incidentRow) scanDest() []any {
	return []any{
		&ir.ID, &ir.DetectedAt, &ir.LastDetectedAt,
		&ir.Severity, &ir.RootCause, &ir.CausalChain,
		&ir.AffectedObjects, &ir.SignalIDs,
		&ir.RecommendedSQL, &ir.ActionRisk,
		&ir.Source, &ir.Confidence, &ir.ResolvedAt,
		&ir.DatabaseName, &ir.OccurrenceCount,
		&ir.EscalatedAt,
	}
}

func (ir *incidentRow) toMap() map[string]any {
	var chain any
	if len(ir.CausalChain) > 0 {
		_ = json.Unmarshal(ir.CausalChain, &chain)
	}
	return map[string]any{
		"id":               ir.ID,
		"detected_at":      ir.DetectedAt,
		"last_detected_at": ir.LastDetectedAt,
		"severity":         ir.Severity,
		"root_cause":       ir.RootCause,
		"causal_chain":     chain,
		"affected_objects": ir.AffectedObjects,
		"signal_ids":       ir.SignalIDs,
		"recommended_sql":  derefStr(ir.RecommendedSQL),
		"action_risk":      derefStr(ir.ActionRisk),
		"source":           ir.Source,
		"confidence":       ir.Confidence,
		"resolved_at":      ir.ResolvedAt,
		"database_name":    derefStr(ir.DatabaseName),
		"occurrence_count": ir.OccurrenceCount,
		"escalated_at":     ir.EscalatedAt,
	}
}

func scanIncidentRows(
	rows pgx.Rows,
) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var ir incidentRow
		if err := rows.Scan(ir.scanDest()...); err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		results = append(results, ir.toMap())
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

func scanIncidentRow(
	row pgx.Row,
) (map[string]any, error) {
	var ir incidentRow
	if err := row.Scan(ir.scanDest()...); err != nil {
		return nil, err
	}
	return ir.toMap(), nil
}

// ---------- Growth forecast queries ----------

const growthForecastSQL = `SELECT sh.metric_type,
 sh.object_name, sh.database_name,
 MIN(sh.size_bytes) AS earliest_bytes,
 MAX(sh.size_bytes) AS latest_bytes,
 COUNT(*) AS data_points,
 MIN(sh.collected_at) AS first_seen,
 MAX(sh.collected_at) AS last_seen
 FROM sage.size_history sh
 WHERE sh.collected_at > now() - ($1 || ' days')::interval
 GROUP BY sh.metric_type, sh.object_name, sh.database_name
 HAVING COUNT(*) >= 2
 ORDER BY MAX(sh.size_bytes) - MIN(sh.size_bytes) DESC
 LIMIT 50`

func queryGrowthForecasts(
	ctx context.Context, pool *pgxpool.Pool, days int,
) ([]map[string]any, error) {
	rows, err := pool.Query(
		ctx, growthForecastSQL, fmt.Sprintf("%d", days),
	)
	if err != nil {
		return nil, fmt.Errorf("query growth forecasts: %w", err)
	}
	defer rows.Close()
	return scanGrowthRows(rows)
}

func scanGrowthRows(
	rows pgx.Rows,
) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			metricType    string
			objectName    string
			databaseName  *string
			earliestBytes int64
			latestBytes   int64
			dataPoints    int
			firstSeen     time.Time
			lastSeen      time.Time
		)
		err := rows.Scan(
			&metricType, &objectName, &databaseName,
			&earliestBytes, &latestBytes, &dataPoints,
			&firstSeen, &lastSeen,
		)
		if err != nil {
			return nil, fmt.Errorf("scan growth row: %w", err)
		}
		growthBytes := latestBytes - earliestBytes
		spanHours := lastSeen.Sub(firstSeen).Hours()
		var dailyRate int64
		if spanHours > 0 {
			dailyRate = int64(
				float64(growthBytes) / spanHours * 24,
			)
		}
		results = append(results, map[string]any{
			"metric_type":           metricType,
			"object_name":           objectName,
			"database_name":         derefStr(databaseName),
			"earliest_bytes":        earliestBytes,
			"latest_bytes":          latestBytes,
			"growth_bytes":          growthBytes,
			"growth_rate_bytes_day": dailyRate,
			"data_points":           dataPoints,
			"first_seen":            firstSeen,
			"last_seen":             lastSeen,
		})
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}
