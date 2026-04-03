package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
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
		if database == "" {
			database = "all"
		}
		filters := parseFindingFilters(q)
		pool := mgr.PoolForDatabase(database)
		displayName := mgr.ResolveDatabaseName(database)
		if pool == nil {
			jsonResponse(w, findingsEmptyResponse(
				displayName, filters,
			))
			return
		}
		findings, total, err := queryFindings(
			r.Context(), pool, filters, displayName,
		)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, map[string]any{
			"database": displayName,
			"filters":  filters,
			"total":    total,
			"offset":   filters.Offset,
			"limit":    filters.Limit,
			"findings": findings,
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
		for _, pool := range mgr.AllPools() {
			finding, err := queryFindingByID(
				r.Context(), pool, id,
			)
			if err == nil {
				jsonResponse(w, finding)
				return
			}
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

func actionsListHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if database == "" {
			database = "all"
		}
		limit := parseIntDefault(q.Get("limit"), 50)
		offset := parseIntDefault(q.Get("offset"), 0)
		if limit > 200 {
			limit = 200
		}
		displayName := mgr.ResolveDatabaseName(database)
		pool := mgr.PoolForDatabase(database)
		if pool == nil {
			jsonResponse(w, map[string]any{
				"database": displayName, "total": 0,
				"offset": offset, "limit": limit,
				"actions": []any{},
			})
			return
		}
		actions, total, err := queryActions(
			r.Context(), pool, limit, offset,
		)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, map[string]any{
			"database": displayName, "total": total,
			"offset": offset, "limit": limit,
			"actions": actions,
		})
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
		pool := mgr.PoolForDatabase("all")
		if pool == nil {
			jsonError(w, "action not found", http.StatusNotFound)
			return
		}
		action, err := queryActionByID(r.Context(), pool, id)
		if err != nil {
			jsonError(w, "action not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, action)
	}
}

func snapshotLatestHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		if database == "" {
			database = "all"
		}
		metric := r.URL.Query().Get("metric")
		if metric == "" {
			metric = "cache_hit_ratio"
		}
		displayName := mgr.ResolveDatabaseName(database)
		pool := mgr.PoolForDatabase(database)
		if pool == nil {
			jsonResponse(w, map[string]any{
				"database": displayName, "snapshot": nil,
			})
			return
		}
		data, err := querySnapshotLatest(
			r.Context(), pool, metric,
		)
		if err != nil {
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
		metric := q.Get("metric")
		if database == "" {
			database = "all"
		}
		if !validateMetric(metric) {
			jsonError(w, "invalid metric", http.StatusBadRequest)
			return
		}
		displayName := mgr.ResolveDatabaseName(database)
		pool := mgr.PoolForDatabase(database)
		if pool == nil {
			jsonResponse(w, map[string]any{
				"database": displayName, "metric": metric,
				"points": []any{},
			})
			return
		}
		hours := parseIntDefault(q.Get("hours"), 24)
		points, err := querySnapshotHistory(
			r.Context(), pool, metric, hours,
		)
		if err != nil {
			jsonResponse(w, map[string]any{
				"database": displayName, "metric": metric,
				"points": []any{},
			})
			return
		}
		jsonResponse(w, map[string]any{
			"database": displayName, "metric": metric,
			"points": points,
		})
	}
}

func validateMetric(metric string) bool {
	if metric == "" {
		return true
	}
	valid := map[string]bool{
		// Collector snapshot categories.
		"tables": true, "indexes": true, "queries": true,
		"sequences": true, "foreign_keys": true, "system": true,
		"io": true, "locks": true, "config_data": true,
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
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
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
		jsonResponse(w, map[string]any{
			"mode":        cfg.Mode,
			"trust":       cfg.Trust,
			"collector":   cfg.Collector,
			"analyzer":    cfg.Analyzer,
			"safety":      cfg.Safety,
			"llm_enabled": cfg.LLM.Enabled,
			"advisor":     cfg.Advisor,
			"databases":   len(cfg.Databases),
		})
	}
}

func configUpdateHandler(
	mgr *fleet.DatabaseManager, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}
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
						return
					}
					cfg.Trust.Level = level
				}
			}
		}
		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func metricsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
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
		database := r.URL.Query().Get("database")
		stopped := mgr.EmergencyStop(database)
		jsonResponse(w, map[string]any{
			"stopped": stopped, "status": "stopped",
		})
	}
}

func resumeHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		resumed := mgr.Resume(database)
		jsonResponse(w, map[string]any{
			"resumed": resumed, "status": "resumed",
		})
	}
}

// --- Query helpers (extracted to keep handlers under 50 lines) ---

func parseFindingFilters(q map[string][]string) fleet.FindingFilters {
	f := fleet.FindingFilters{
		Status:   valOrDefault(q, "status", "open"),
		Severity: valOrDefault(q, "severity", ""),
		Category: valOrDefault(q, "category", ""),
		Sort:     valOrDefault(q, "sort", "severity"),
		Order:    valOrDefault(q, "order", "desc"),
		Limit:    parseIntDefault(firstVal(q, "limit"), 50),
		Offset:   parseIntDefault(firstVal(q, "offset"), 0),
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	return f
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
		"database": database,
		"filters":  filters,
		"total":    0,
		"offset":   filters.Offset,
		"limit":    filters.Limit,
		"findings": []any{},
	}
}

func queryFindings(
	ctx context.Context, pool *pgxpool.Pool,
	f fleet.FindingFilters, database string,
) ([]map[string]any, int, error) {
	where, args := buildFindingsWhere(f)
	countQ := "SELECT COUNT(*) FROM sage.findings" + where
	var total int
	if err := pool.QueryRow(ctx, countQ, args...).Scan(
		&total,
	); err != nil {
		return nil, 0, fmt.Errorf("count findings: %w", err)
	}
	selectQ := findingsSelectSQL + where +
		buildFindingsOrder(f) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d",
			len(args)+1, len(args)+2)
	args = append(args, f.Limit, f.Offset)
	rows, err := pool.Query(ctx, selectQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query findings: %w", err)
	}
	defer rows.Close()
	findings, err := scanFindingRows(rows, database)
	if err != nil {
		return nil, 0, err
	}
	return findings, total, nil
}

const findingsSelectSQL = `SELECT id, created_at, last_seen,
 occurrence_count, category, severity, object_type,
 object_identifier, title, detail, recommendation,
 recommended_sql, status FROM sage.findings`

func buildFindingsWhere(
	f fleet.FindingFilters,
) (string, []any) {
	where := " WHERE 1=1"
	var args []any
	n := 1
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
	}
	return where, args
}

func buildFindingsOrder(f fleet.FindingFilters) string {
	dir := "DESC"
	if f.Order == "asc" {
		dir = "ASC"
	}
	if f.Sort == "severity" {
		// For severity, "desc" means most severe first (critical→warning→info),
		// which is ASC on the CASE values (critical=1, warning=2, info=3).
		sevDir := "ASC"
		if f.Order == "asc" {
			sevDir = "DESC"
		}
		return fmt.Sprintf(
			" ORDER BY CASE severity"+
				" WHEN 'critical' THEN 1"+
				" WHEN 'warning' THEN 2"+
				" WHEN 'info' THEN 3"+
				" ELSE 4 END %s", sevDir)
	}
	// Allowlist sort columns to prevent injection
	col := "last_seen"
	allowed := map[string]string{
		"created_at": "created_at",
		"last_seen":  "last_seen",
		"category":   "category",
		"title":      "title",
	}
	if c, ok := allowed[f.Sort]; ok {
		col = c
	}
	return fmt.Sprintf(" ORDER BY %s %s", col, dir)
}

func scanFindingRows(
	rows pgx.Rows, database string,
) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			id              int64
			createdAt       time.Time
			lastSeen        time.Time
			occurrenceCount int
			category        string
			severity        string
			objectType      *string
			objectIdent     *string
			title           string
			detail          []byte
			recommendation  *string
			recommendedSQL  *string
			status          string
		)
		err := rows.Scan(
			&id, &createdAt, &lastSeen, &occurrenceCount,
			&category, &severity, &objectType, &objectIdent,
			&title, &detail, &recommendation, &recommendedSQL,
			&status,
		)
		if err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		f := buildFindingMap(
			id, createdAt, lastSeen, occurrenceCount,
			category, severity, objectType, objectIdent,
			title, detail, recommendation, recommendedSQL,
			status, database,
		)
		results = append(results, f)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

func buildFindingMap(
	id int64, createdAt, lastSeen time.Time,
	occurrenceCount int, category, severity string,
	objectType, objectIdent *string, title string,
	detail []byte, recommendation, recommendedSQL *string,
	status, database string,
) map[string]any {
	var detailParsed any
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &detailParsed)
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
		"status":            status,
		"database_name":     database,
	}
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
	var detailParsed any
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &detailParsed)
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

const findingDetailSQL = `SELECT id, created_at, last_seen,
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
		`UPDATE sage.findings SET status = $1
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
				`DELETE FROM sage.findings
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

func queryActions(
	ctx context.Context, pool *pgxpool.Pool,
	limit, offset int,
) ([]map[string]any, int, error) {
	var total int
	err := pool.QueryRow(
		ctx, "SELECT COUNT(*) FROM sage.action_log",
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count actions: %w", err)
	}
	rows, err := pool.Query(ctx, actionsSelectSQL,
		limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query actions: %w", err)
	}
	defer rows.Close()
	actions, err := scanActionRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return actions, total, nil
}

const actionsSelectSQL = `SELECT id, executed_at,
 action_type, finding_id, sql_executed, rollback_sql,
 before_state, after_state, outcome, rollback_reason,
 measured_at FROM sage.action_log
 ORDER BY executed_at DESC LIMIT $1 OFFSET $2`

func scanActionRows(rows pgx.Rows) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			id             int64
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
		err := rows.Scan(
			&id, &executedAt, &actionType, &findingID,
			&sqlExecuted, &rollbackSQL, &beforeState,
			&afterState, &outcome, &rollbackReason,
			&measuredAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		a := buildActionMap(
			id, executedAt, actionType, findingID,
			sqlExecuted, rollbackSQL, beforeState,
			afterState, outcome, rollbackReason, measuredAt,
		)
		results = append(results, a)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

func buildActionMap(
	id int64, executedAt time.Time, actionType string,
	findingID *int64, sqlExecuted string,
	rollbackSQL *string, beforeState, afterState []byte,
	outcome string, rollbackReason *string,
	measuredAt *time.Time,
) map[string]any {
	var before, after any
	if len(beforeState) > 0 {
		_ = json.Unmarshal(beforeState, &before)
	}
	if len(afterState) > 0 {
		_ = json.Unmarshal(afterState, &after)
	}
	var fID *string
	if findingID != nil {
		s := strconv.FormatInt(*findingID, 10)
		fID = &s
	}
	return map[string]any{
		"id":              strconv.FormatInt(id, 10),
		"executed_at":     executedAt,
		"action_type":     actionType,
		"finding_id":      fID,
		"sql_executed":    sqlExecuted,
		"rollback_sql":    derefStr(rollbackSQL),
		"before_state":    before,
		"after_state":     after,
		"outcome":         outcome,
		"rollback_reason": derefStr(rollbackReason),
		"measured_at":     measuredAt,
	}
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

const actionDetailSQL = `SELECT id, executed_at,
 action_type, finding_id, sql_executed, rollback_sql,
 before_state, after_state, outcome, rollback_reason,
 measured_at FROM sage.action_log WHERE id = $1`

func querySnapshotLatest(
	ctx context.Context, pool *pgxpool.Pool, metric string,
) (any, error) {
	var data []byte
	err := pool.QueryRow(ctx,
		`SELECT data FROM sage.snapshots
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

func querySnapshotHistory(
	ctx context.Context, pool *pgxpool.Pool,
	metric string, hours int,
) ([]map[string]any, error) {
	rows, err := pool.Query(ctx,
		`SELECT collected_at, data FROM sage.snapshots
		 WHERE category = $1
		 AND collected_at > now() - ($2 || ' hours')::interval
		 ORDER BY collected_at`,
		metric, strconv.Itoa(hours),
	)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	var points []map[string]any
	for rows.Next() {
		var (
			ts   time.Time
			data []byte
		)
		if err := rows.Scan(&ts, &data); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		var parsed any
		_ = json.Unmarshal(data, &parsed)
		points = append(points, map[string]any{
			"timestamp": ts, "data": parsed,
		})
	}
	if points == nil {
		points = []map[string]any{}
	}
	return points, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
