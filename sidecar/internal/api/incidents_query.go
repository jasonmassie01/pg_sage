package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const incidentsBaseSQL = `/* pg_sage */SELECT id, detected_at,
 COALESCE(last_detected_at, detected_at) AS last_detected_at,
 severity, root_cause, causal_chain, affected_objects, signal_ids,
 recommended_sql, action_risk, source, confidence,
 resolved_at, database_name, occurrence_count, escalated_at,
 rollback_sql, resolved_by, resolution_reason,
 previous_incident_id::text
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
	return queryActiveIncidentsLimit(ctx, pool, dbName, 100)
}

// queryActiveIncidentsLimit reads up to limit open incidents, most severe
// and then most recent first.
func queryActiveIncidentsLimit(
	ctx context.Context, pool *pgxpool.Pool, dbName string, limit int,
) ([]map[string]any, error) {
	where := " WHERE resolved_at IS NULL"
	args := []any{limit}
	if dbName != "" {
		where += " AND database_name = $2"
		args = append(args, dbName)
	}
	query := incidentsBaseSQL + where +
		` ORDER BY CASE severity
		    WHEN 'critical' THEN 1
		    WHEN 'warning' THEN 2
		    ELSE 3 END, detected_at DESC LIMIT $1`
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
	RollbackSQL     *string
	ResolvedBy      *string
	Reason          *string
	PreviousID      *string
}

func (ir *incidentRow) scanDest() []any {
	return []any{
		&ir.ID, &ir.DetectedAt, &ir.LastDetectedAt,
		&ir.Severity, &ir.RootCause, &ir.CausalChain,
		&ir.AffectedObjects, &ir.SignalIDs,
		&ir.RecommendedSQL, &ir.ActionRisk,
		&ir.Source, &ir.Confidence, &ir.ResolvedAt,
		&ir.DatabaseName, &ir.OccurrenceCount,
		&ir.EscalatedAt, &ir.RollbackSQL, &ir.ResolvedBy,
		&ir.Reason, &ir.PreviousID,
	}
}

func (ir *incidentRow) toMap() map[string]any {
	var chain any
	if len(ir.CausalChain) > 0 {
		_ = json.Unmarshal(ir.CausalChain, &chain)
	}
	return map[string]any{
		"id":                   ir.ID,
		"detected_at":          ir.DetectedAt,
		"last_detected_at":     ir.LastDetectedAt,
		"severity":             ir.Severity,
		"root_cause":           ir.RootCause,
		"causal_chain":         chain,
		"affected_objects":     ir.AffectedObjects,
		"signal_ids":           ir.SignalIDs,
		"recommended_sql":      derefStr(ir.RecommendedSQL),
		"action_risk":          derefStr(ir.ActionRisk),
		"source":               ir.Source,
		"confidence":           ir.Confidence,
		"resolved_at":          ir.ResolvedAt,
		"database_name":        derefStr(ir.DatabaseName),
		"occurrence_count":     ir.OccurrenceCount,
		"escalated_at":         ir.EscalatedAt,
		"rollback_sql":         derefStr(ir.RollbackSQL),
		"resolved_by":          derefStr(ir.ResolvedBy),
		"resolution_reason":    derefStr(ir.Reason),
		"previous_incident_id": derefStr(ir.PreviousID),
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
	return scannedRows(rows, results, "incidents")
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
