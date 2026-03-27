package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxToolInputLen = 10000

// ---------------------------------------------------------------------------
// Tool catalogue
// ---------------------------------------------------------------------------

var toolCatalogue = []Tool{
	{
		Name:        "diagnose",
		Description: "Ask pg_sage an interactive diagnostic question about your database",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"question": {"type": "string", "description": "The diagnostic question to ask pg_sage"}
			},
			"required": ["question"]
		}`),
	},
	{
		Name:        "briefing",
		Description: "Generate a health briefing of the current database state",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
	},
	{
		Name:        "suggest_index",
		Description: "Get index suggestions for a specific table",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"table": {"type": "string", "description": "Table name (optionally schema-qualified)"}
			},
			"required": ["table"]
		}`),
	},
	{
		Name:        "review_migration",
		Description: "Review DDL / migration SQL for risks and issues",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"ddl": {"type": "string", "description": "The DDL / migration SQL to review"}
			},
			"required": ["ddl"]
		}`),
	},
	{
		Name:        "forecast",
		Description: "Get workload forecasts — disk growth, connection saturation, cache pressure, sequence exhaustion, and more",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"category": {
					"type": "string",
					"description": "Filter by forecast category (e.g. forecast_disk_growth, forecast_connection_saturation). Omit for all forecasts."
				}
			},
			"required": []
		}`),
	},
	{
		Name:        "query_hints",
		Description: "View active per-query performance hints applied by pg_sage",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"queryid": {
					"type": "integer",
					"description": "Filter by specific query ID. Omit for all active hints."
				}
			},
			"required": []
		}`),
	},
}

// ---------------------------------------------------------------------------
// Tool dispatcher
// ---------------------------------------------------------------------------

// Per-tool timeouts. LLM-backed tools get 120s; others 30s.
var toolTimeouts = map[string]time.Duration{
	"diagnose":         120 * time.Second,
	"briefing":         120 * time.Second,
	"suggest_index":    120 * time.Second,
	"review_migration": 120 * time.Second,
	"forecast":         30 * time.Second,
	"query_hints":      30 * time.Second,
}

func callTool(ctx context.Context, name string, args json.RawMessage) (ToolsCallResult, error) {
	if timeout, ok := toolTimeouts[name]; ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	switch name {
	case "diagnose":
		return toolDiagnose(ctx, args)
	case "briefing":
		return toolBriefing(ctx)
	case "suggest_index":
		return toolSuggestIndex(ctx, args)
	case "review_migration":
		return toolReviewMigration(ctx, args)
	case "forecast":
		return toolForecast(ctx, args)
	case "query_hints":
		return toolQueryHints(ctx, args)
	default:
		return ToolsCallResult{
			Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("unknown tool: %s", name)}},
			IsError: true,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// Individual tool implementations
// ---------------------------------------------------------------------------

func toolDiagnose(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid arguments: " + err.Error()), nil
	}
	if p.Question == "" {
		return toolErr("question is required"), nil
	}
	if len(p.Question) > maxToolInputLen {
		return toolErr("question too long (max 10000 chars)"), nil
	}

	if !extensionAvailable {
		return toolErr("The 'diagnose' tool requires the pg_sage extension to be installed. " +
			"This sidecar is running in sidecar-only mode against a PostgreSQL instance without pg_sage. " +
			"You can still use: sage://health, sage://schema/{table}, sage://stats/{table}, sage://slow-queries resources, " +
			"and the suggest_index / review_migration tools."), nil
	}

	result, err := queryTextFallback(ctx, "tool:diagnose",
		"SELECT sage.diagnose($1)", []any{p.Question},
		"SELECT 'sage.diagnose() not available — ensure pg_sage extension is loaded'", nil,
	)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	return toolOK(result), nil
}

func toolBriefing(ctx context.Context) (ToolsCallResult, error) {
	if !extensionAvailable {
		return toolErr("The 'briefing' tool requires the pg_sage extension to be installed. " +
			"This sidecar is running in sidecar-only mode. " +
			"Use the sage://health resource for a basic health overview instead."), nil
	}

	result, err := queryTextFallback(ctx, "tool:briefing",
		"SELECT sage.briefing()", nil,
		"SELECT 'sage.briefing() not available — ensure pg_sage extension is loaded'", nil,
	)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	return toolOK(result), nil
}

func toolSuggestIndex(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		Table string `json:"table"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid arguments: " + err.Error()), nil
	}
	if p.Table == "" {
		return toolErr("table is required"), nil
	}

	if extensionAvailable {
		question := "suggest indexes for table " + sanitize(p.Table)
		result, err := queryTextFallback(ctx, "tool:suggest_index",
			"SELECT sage.diagnose($1)", []any{question},
			"SELECT 'sage.diagnose() not available'", nil,
		)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK(result), nil
	}

	// Sidecar-only: analyze pg_stat_user_tables for sequential scan patterns
	t := sanitize(p.Table)
	var result string
	err := pool.QueryRow(ctx, `SELECT json_build_object(
		'table', $1::text,
		'mode', 'sidecar-only',
		'analysis', json_build_object(
			'seq_scan', s.seq_scan,
			'seq_tup_read', s.seq_tup_read,
			'idx_scan', coalesce(s.idx_scan, 0),
			'idx_tup_fetch', coalesce(s.idx_tup_fetch, 0),
			'n_live_tup', s.n_live_tup,
			'seq_scan_ratio', CASE WHEN (s.seq_scan + coalesce(s.idx_scan,0)) > 0
				THEN round(s.seq_scan::numeric / (s.seq_scan + coalesce(s.idx_scan,0)), 4)
				ELSE 0 END
		),
		'existing_indexes', (
			SELECT coalesce(json_agg(json_build_object('name', indexname, 'def', indexdef)), '[]'::json)
			FROM pg_indexes
			WHERE schemaname || '.' || tablename = $1::text OR tablename = $1::text
		),
		'recommendation', CASE
			WHEN s.seq_scan > 1000 AND s.n_live_tup > 10000 AND coalesce(s.idx_scan,0) < s.seq_scan
			THEN 'High sequential scan count on a large table. Consider adding indexes on columns used in WHERE, JOIN, and ORDER BY clauses.'
			WHEN s.seq_scan > 100 AND s.n_live_tup > 1000
			THEN 'Moderate sequential scans detected. Review query patterns and consider targeted indexes.'
			ELSE 'Sequential scan counts appear reasonable for the table size. No urgent index recommendations.'
		END
	)::text
	FROM pg_stat_user_tables s
	WHERE s.schemaname || '.' || s.relname = $1::text OR s.relname = $1::text
	LIMIT 1`, t).Scan(&result)
	if err != nil {
		return toolErr(fmt.Sprintf("could not analyze table %s: %v", t, err)), nil
	}
	return toolOK(result), nil
}

func toolReviewMigration(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		DDL string `json:"ddl"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid arguments: " + err.Error()), nil
	}
	if p.DDL == "" {
		return toolErr("ddl is required"), nil
	}
	if len(p.DDL) > maxToolInputLen {
		return toolErr("ddl too long (max 10000 chars)"), nil
	}

	if extensionAvailable {
		question := "review this migration: " + p.DDL
		result, err := queryTextFallback(ctx, "tool:review_migration",
			"SELECT sage.diagnose($1)", []any{question},
			"SELECT 'sage.diagnose() not available'", nil,
		)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK(result), nil
	}

	// Sidecar-only: basic DDL safety checks
	review := reviewDDLSafety(p.DDL)
	out, _ := json.Marshal(review)
	return toolOK(string(out)), nil
}

// reviewDDLSafety performs basic static analysis of DDL statements for common risks.
func reviewDDLSafety(ddl string) map[string]any {
	upper := strings.ToUpper(ddl)
	var warnings []string

	if strings.Contains(upper, "DROP TABLE") {
		warnings = append(warnings, "DROP TABLE detected: this will permanently delete the table and all its data.")
	}
	if strings.Contains(upper, "DROP COLUMN") {
		warnings = append(warnings, "DROP COLUMN detected: this is irreversible and may break applications referencing this column.")
	}
	if strings.Contains(upper, "ALTER TABLE") && strings.Contains(upper, "ALTER COLUMN") && strings.Contains(upper, "TYPE") {
		warnings = append(warnings, "Column type change detected: this may require a full table rewrite and lock the table for the duration.")
	}
	if strings.Contains(upper, "NOT NULL") && !strings.Contains(upper, "CREATE TABLE") {
		warnings = append(warnings, "Adding NOT NULL constraint: on large tables this requires a full table scan. Consider adding with a DEFAULT or using a CHECK constraint first.")
	}
	if strings.Contains(upper, "ADD COLUMN") && strings.Contains(upper, "DEFAULT") && !strings.Contains(upper, "CREATE TABLE") {
		warnings = append(warnings, "Adding column with DEFAULT: in PostgreSQL 11+ this is fast (metadata only). Verify your PostgreSQL version.")
	}
	if strings.Contains(upper, "CREATE INDEX") && !strings.Contains(upper, "CONCURRENTLY") {
		warnings = append(warnings, "CREATE INDEX without CONCURRENTLY: this will lock the table for writes during index creation. Consider using CREATE INDEX CONCURRENTLY.")
	}
	if strings.Contains(upper, "LOCK TABLE") || strings.Contains(upper, "ACCESS EXCLUSIVE") {
		warnings = append(warnings, "Explicit table lock detected: this will block all other access to the table.")
	}
	if strings.Contains(upper, "TRUNCATE") {
		warnings = append(warnings, "TRUNCATE detected: this removes all rows and is not MVCC-safe. It acquires an ACCESS EXCLUSIVE lock.")
	}
	if strings.Contains(upper, "RENAME") {
		warnings = append(warnings, "RENAME detected: this may break application queries referencing the old name.")
	}
	if strings.Contains(upper, "SET DATA TYPE") || strings.Contains(upper, "USING") {
		warnings = append(warnings, "Type conversion with USING detected: verify the conversion expression is correct for all existing data.")
	}

	if len(warnings) == 0 {
		warnings = append(warnings, "No obvious risks detected in the DDL. Standard review still recommended before production deployment.")
	}

	return map[string]any{
		"mode":     "sidecar-only",
		"ddl":      ddl,
		"warnings": warnings,
		"note":     "This is a basic static analysis. Install the pg_sage extension for deeper migration review with AI-powered analysis.",
	}
}

func toolForecast(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		Category string `json:"category"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &p)
	}

	query := `SELECT coalesce(
		(SELECT json_agg(json_build_object(
			'category', category,
			'severity', severity,
			'title', title,
			'object_identifier', object_identifier,
			'detail', detail,
			'occurrence_count', occurrence_count,
			'last_seen', last_seen
		) ORDER BY
			CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			last_seen DESC
		)
		FROM sage.findings
		WHERE status = 'open'
		  AND category LIKE 'forecast_%'`

	var result string
	var err error

	if p.Category != "" {
		query += ` AND category = $1), '[]'::json)::text`
		err = pool.QueryRow(ctx, query, sanitize(p.Category)).Scan(&result)
	} else {
		query += `), '[]'::json)::text`
		err = pool.QueryRow(ctx, query).Scan(&result)
	}
	if err != nil {
		return toolErr(fmt.Sprintf("forecast query failed: %v", err)), nil
	}
	return toolOK(result), nil
}

func toolQueryHints(
	ctx context.Context, args json.RawMessage,
) (ToolsCallResult, error) {
	var p struct {
		QueryID *int64 `json:"queryid"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &p)
	}

	query := `SELECT coalesce(
		(SELECT json_agg(json_build_object(
			'queryid', queryid,
			'hint_text', hint_text,
			'symptom', symptom,
			'status', status,
			'created_at', created_at,
			'before_cost', before_cost,
			'after_cost', after_cost
		) ORDER BY created_at DESC)
		FROM sage.query_hints
		WHERE status = 'active'`

	var result string
	var err error
	if p.QueryID != nil {
		query += ` AND queryid = $1), '[]'::json)::text`
		err = pool.QueryRow(
			ctx, query, *p.QueryID,
		).Scan(&result)
	} else {
		query += `), '[]'::json)::text`
		err = pool.QueryRow(ctx, query).Scan(&result)
	}
	if err != nil {
		return toolErr(
			fmt.Sprintf("query_hints query failed: %v", err),
		), nil
	}
	return toolOK(result), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func queryTextFallback(ctx context.Context, component, primary string, primaryArgs []any, fallback string, fallbackArgs []any) (string, error) {
	var result string
	err := pool.QueryRow(ctx, primary, primaryArgs...).Scan(&result)
	if err == nil {
		return result, nil
	}
	logWarn(component, "primary query failed: %v, trying fallback", err)
	if fallbackArgs == nil {
		fallbackArgs = []any{}
	}
	err2 := pool.QueryRow(ctx, fallback, fallbackArgs...).Scan(&result)
	if err2 != nil {
		logError(component, "fallback also failed: %v", err2)
		return "", fmt.Errorf("%s: primary: %v; fallback: %v", component, err, err2)
	}
	return result, nil
}

func toolOK(text string) ToolsCallResult {
	return ToolsCallResult{Content: []ToolContent{{Type: "text", Text: text}}}
}

func toolErr(text string) ToolsCallResult {
	return ToolsCallResult{Content: []ToolContent{{Type: "text", Text: text}}, IsError: true}
}

func unmarshalToolsCall(raw json.RawMessage) (string, json.RawMessage, error) {
	var p ToolsCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, err
	}
	if p.Name == "" {
		return "", nil, fmt.Errorf("tool name is required")
	}
	return p.Name, p.Arguments, nil
}
