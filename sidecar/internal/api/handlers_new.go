package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func forecastsHandler(
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
		var forecasts []map[string]any
		for _, selected := range pools {
			dbForecasts, err := queryForecasts(
				r.Context(), selected.pool)
			if err != nil {
				slog.Error("query forecasts failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query forecasts", 500)
				return
			}
			for _, forecast := range dbForecasts {
				forecast["database_name"] = selected.name
				forecasts = append(forecasts, forecast)
			}
		}
		sort.SliceStable(forecasts, func(i, j int) bool {
			return timeFromMap(forecasts[i], "last_seen").After(
				timeFromMap(forecasts[j], "last_seen"))
		})
		if forecasts == nil {
			forecasts = []map[string]any{}
		}
		jsonResponse(w, map[string]any{
			"database":  displayName,
			"forecasts": forecasts,
		})
	}
}

func queryHintsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		status := r.URL.Query().Get("status")
		if status != "" && status != "active" &&
			status != "retired" && status != "broken" {
			jsonError(w, "invalid status", http.StatusBadRequest)
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
				"database": displayName,
				"hints":    []any{},
			})
			return
		}
		var hints []map[string]any
		for _, selected := range pools {
			dbHints, err := queryQueryHints(
				r.Context(), selected.pool, status)
			if err != nil {
				slog.Error("query hints failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query hints", 500)
				return
			}
			for _, hint := range dbHints {
				hint["database_name"] = selected.name
				hints = append(hints, hint)
			}
		}
		sort.SliceStable(hints, func(i, j int) bool {
			return timeFromMap(hints[i], "created_at").After(
				timeFromMap(hints[j], "created_at"))
		})
		if hints == nil {
			hints = []map[string]any{}
		}
		jsonResponse(w, map[string]any{
			"database": displayName,
			"hints":    hints,
		})
	}
}

func alertLogHandler(
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
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		displayName := responseDatabaseName(database)
		pools := poolsForDatabaseSelection(mgr, database)
		if len(pools) == 0 {
			jsonResponse(w, map[string]any{
				"database": displayName,
				"alerts":   []any{},
			})
			return
		}
		var alerts []map[string]any
		for _, selected := range pools {
			dbAlerts, err := queryAlertLog(r.Context(), selected.pool)
			if err != nil {
				slog.Error("query alert log failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query alerts", 500)
				return
			}
			for _, alert := range dbAlerts {
				alert["database_name"] = selected.name
				alerts = append(alerts, alert)
			}
		}
		sort.SliceStable(alerts, func(i, j int) bool {
			return timeFromMap(alerts[i], "sent_at").After(
				timeFromMap(alerts[j], "sent_at"))
		})
		if alerts == nil {
			alerts = []map[string]any{}
		}
		jsonResponse(w, map[string]any{
			"database": displayName,
			"alerts":   alerts,
		})
	}
}

// --- Query helpers ---

const forecastsSQL = `SELECT category, severity, title,
 object_identifier, detail, occurrence_count, last_seen
 FROM sage.findings
 WHERE status = 'open' AND category LIKE 'forecast_%'
 ORDER BY CASE severity
   WHEN 'critical' THEN 1
   WHEN 'warning' THEN 2
   WHEN 'info' THEN 3
   ELSE 4 END, last_seen DESC`

func queryForecasts(
	ctx context.Context, pool *pgxpool.Pool,
) ([]map[string]any, error) {
	rows, err := pool.Query(ctx, forecastsSQL)
	if err != nil {
		return nil, fmt.Errorf("query forecasts: %w", err)
	}
	defer rows.Close()
	return scanForecastRows(rows)
}

func scanForecastRows(rows pgx.Rows) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			category        string
			severity        string
			title           string
			objectIdent     *string
			detail          []byte
			occurrenceCount int
			lastSeen        time.Time
		)
		err := rows.Scan(
			&category, &severity, &title,
			&objectIdent, &detail, &occurrenceCount,
			&lastSeen,
		)
		if err != nil {
			return nil, fmt.Errorf("scan forecast: %w", err)
		}
		var detailParsed any
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &detailParsed)
		}
		results = append(results, map[string]any{
			"category":          category,
			"severity":          severity,
			"title":             title,
			"object_identifier": derefStr(objectIdent),
			"detail":            detailParsed,
			"occurrence_count":  occurrenceCount,
			"last_seen":         lastSeen,
		})
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

const queryHintsSQL = `SELECT queryid, hint_text, symptom,
 status, created_at, before_cost, after_cost,
 COALESCE(suggested_rewrite, '') AS suggested_rewrite,
 COALESCE(rewrite_rationale, '') AS rewrite_rationale,
 verified_at, rolled_back_at
 FROM sage.query_hints`

func queryQueryHints(
	ctx context.Context, pool *pgxpool.Pool, status ...string,
) ([]map[string]any, error) {
	query := queryHintsSQL
	var args []any
	if len(status) > 0 && status[0] != "" {
		query += " WHERE status = $1"
		args = append(args, status[0])
	}
	query += " ORDER BY created_at DESC LIMIT 200"
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query hints: %w", err)
	}
	defer rows.Close()
	return scanQueryHintRows(rows)
}

func scanQueryHintRows(rows pgx.Rows) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			queryID          int64
			hintText         string
			symptom          string
			status           string
			createdAt        time.Time
			beforeCost       *float64
			afterCost        *float64
			suggestedRewrite string
			rewriteRationale string
			verifiedAt       *time.Time
			rolledBackAt     *time.Time
		)
		err := rows.Scan(
			&queryID, &hintText, &symptom,
			&status, &createdAt, &beforeCost, &afterCost,
			&suggestedRewrite, &rewriteRationale,
			&verifiedAt, &rolledBackAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan query hint: %w", err)
		}
		row := map[string]any{
			"queryid":     queryID,
			"hint_text":   hintText,
			"symptom":     symptom,
			"status":      status,
			"created_at":  createdAt,
			"before_cost": beforeCost,
			"after_cost":  afterCost,
		}
		if verifiedAt != nil {
			row["verified_at"] = verifiedAt
		}
		if rolledBackAt != nil {
			row["rolled_back_at"] = rolledBackAt
		}
		if suggestedRewrite != "" {
			row["suggested_rewrite"] = suggestedRewrite
			row["rewrite_rationale"] = rewriteRationale
		}
		results = append(results, row)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

func timeFromMap(row map[string]any, key string) time.Time {
	v := row[key]
	switch t := v.(type) {
	case time.Time:
		return t
	case *time.Time:
		if t != nil {
			return *t
		}
	}
	return time.Time{}
}

const alertLogSQL = `SELECT a.id, a.sent_at, a.severity,
 a.channel, a.status, a.error_message,
 COALESCE(f.category, '') AS category,
 COALESCE(f.title, '') AS title
 FROM sage.alert_log a
 LEFT JOIN sage.findings f ON f.id = a.finding_id
 ORDER BY a.sent_at DESC LIMIT 100`

func queryAlertLog(
	ctx context.Context, pool *pgxpool.Pool,
) ([]map[string]any, error) {
	rows, err := pool.Query(ctx, alertLogSQL)
	if err != nil {
		return nil, fmt.Errorf("query alert log: %w", err)
	}
	defer rows.Close()
	return scanAlertLogRows(rows)
}

func scanAlertLogRows(rows pgx.Rows) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			id           int64
			sentAt       time.Time
			severity     string
			channel      string
			status       string
			errorMessage *string
			category     string
			title        string
		)
		err := rows.Scan(
			&id, &sentAt, &severity,
			&channel, &status, &errorMessage,
			&category, &title,
		)
		if err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		results = append(results, map[string]any{
			"id":            id,
			"sent_at":       sentAt,
			"severity":      severity,
			"channel":       channel,
			"status":        status,
			"error_message": derefStr(errorMessage),
			"category":      category,
			"title":         title,
		})
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}
