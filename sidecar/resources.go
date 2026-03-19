package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Resource catalogue
// ---------------------------------------------------------------------------

var resourceCatalogue = []Resource{
	{URI: "sage://health", Name: "Database Health", Description: "Current health snapshot from pg_sage", MimeType: "application/json"},
	{URI: "sage://findings", Name: "Open Findings", Description: "All open findings from pg_sage", MimeType: "application/json"},
	{URI: "sage://slow-queries", Name: "Slow Queries", Description: "Recently observed slow queries", MimeType: "application/json"},
	{URI: "sage://schema/{table}", Name: "Table Schema", Description: "Column and index info for a table", MimeType: "application/json"},
	{URI: "sage://stats/{table}", Name: "Table Statistics", Description: "pg_stat_user_tables stats for a table", MimeType: "application/json"},
	{URI: "sage://explain/{queryid}", Name: "Query Plan", Description: "Cached EXPLAIN plan for a query ID", MimeType: "application/json"},
}

// ---------------------------------------------------------------------------
// Read dispatcher
// ---------------------------------------------------------------------------

func readResource(ctx context.Context, uri string) (ResourcesReadResult, error) {
	var text string
	var err error

	switch {
	case uri == "sage://health":
		text, err = readHealth(ctx)
	case uri == "sage://findings":
		text, err = readFindings(ctx)
	case uri == "sage://slow-queries":
		text, err = readSlowQueries(ctx)
	case strings.HasPrefix(uri, "sage://schema/"):
		table := strings.TrimPrefix(uri, "sage://schema/")
		text, err = readSchema(ctx, table)
	case strings.HasPrefix(uri, "sage://stats/"):
		table := strings.TrimPrefix(uri, "sage://stats/")
		text, err = readStats(ctx, table)
	case strings.HasPrefix(uri, "sage://explain/"):
		qid := strings.TrimPrefix(uri, "sage://explain/")
		text, err = readExplain(ctx, qid)
	default:
		return ResourcesReadResult{}, fmt.Errorf("unknown resource URI: %s", uri)
	}

	if err != nil {
		return ResourcesReadResult{}, err
	}
	return ResourcesReadResult{
		Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: text}},
	}, nil
}

// ---------------------------------------------------------------------------
// Individual resource handlers — try SQL function first, then fallback
// ---------------------------------------------------------------------------

func readHealth(ctx context.Context) (string, error) {
	return queryJSONFallback(ctx,
		"SELECT sage.health_json()",
		`SELECT json_build_object(
			'status', 'ok',
			'connections', (SELECT count(*) FROM pg_stat_activity),
			'uptime', (SELECT extract(epoch FROM now() - pg_postmaster_start_time())::int),
			'pg_version', version()
		)::text`,
	)
}

func readFindings(ctx context.Context) (string, error) {
	return queryJSONFallback(ctx,
		"SELECT sage.findings_json('open')",
		`SELECT coalesce(
			(SELECT json_agg(row_to_json(f))
			 FROM sage.findings f
			 WHERE f.status = 'open'),
			'[]'::json
		)::text`,
	)
}

func readSlowQueries(ctx context.Context) (string, error) {
	return queryJSONFallback(ctx,
		"SELECT sage.slow_queries_json()",
		`SELECT coalesce(
			(SELECT json_agg(row_to_json(s))
			 FROM (
				SELECT queryid, query, calls, mean_exec_time, total_exec_time
				FROM pg_stat_statements
				ORDER BY mean_exec_time DESC
				LIMIT 20
			 ) s),
			'[]'::json
		)::text`,
	)
}

func readSchema(ctx context.Context, table string) (string, error) {
	return queryJSONFallback(ctx,
		fmt.Sprintf("SELECT sage.schema_json('%s')", sanitize(table)),
		fmt.Sprintf(`SELECT json_build_object(
			'table', '%s',
			'columns', (
				SELECT json_agg(json_build_object(
					'name', column_name,
					'type', data_type,
					'nullable', is_nullable
				))
				FROM information_schema.columns
				WHERE table_schema || '.' || table_name = '%s'
				   OR table_name = '%s'
			),
			'indexes', (
				SELECT json_agg(json_build_object(
					'name', indexname,
					'def', indexdef
				))
				FROM pg_indexes
				WHERE schemaname || '.' || tablename = '%s'
				   OR tablename = '%s'
			)
		)::text`, sanitize(table), sanitize(table), sanitize(table), sanitize(table), sanitize(table)),
	)
}

func readStats(ctx context.Context, table string) (string, error) {
	return queryJSONFallback(ctx,
		fmt.Sprintf("SELECT sage.stats_json('%s')", sanitize(table)),
		fmt.Sprintf(`SELECT row_to_json(s)::text
		FROM (
			SELECT relname, seq_scan, seq_tup_read, idx_scan, idx_tup_fetch,
			       n_tup_ins, n_tup_upd, n_tup_del, n_live_tup, n_dead_tup,
			       last_vacuum, last_autovacuum, last_analyze, last_autoanalyze
			FROM pg_stat_user_tables
			WHERE schemaname || '.' || relname = '%s'
			   OR relname = '%s'
			LIMIT 1
		) s`, sanitize(table), sanitize(table)),
	)
}

func readExplain(ctx context.Context, queryid string) (string, error) {
	return queryJSONFallback(ctx,
		fmt.Sprintf("SELECT sage.explain_json('%s')", sanitize(queryid)),
		fmt.Sprintf(`SELECT coalesce(
			(SELECT plan::text FROM sage.explain_cache WHERE queryid = '%s' ORDER BY captured_at DESC LIMIT 1),
			'{"error":"no cached plan found"}'
		)`, sanitize(queryid)),
	)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// queryJSONFallback tries the primary query; on failure tries the fallback.
func queryJSONFallback(ctx context.Context, primary, fallback string) (string, error) {
	var result string
	err := pool.QueryRow(ctx, primary).Scan(&result)
	if err == nil {
		return result, nil
	}
	err2 := pool.QueryRow(ctx, fallback).Scan(&result)
	if err2 != nil {
		return "", fmt.Errorf("primary: %v; fallback: %v", err, err2)
	}
	return result, nil
}

// sanitize does basic SQL identifier sanitisation to prevent injection.
func sanitize(s string) string {
	// Allow only alphanumerics, underscores, dots, and hyphens
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unmarshalResourcesRead extracts URI from params.
func unmarshalResourcesRead(raw json.RawMessage) (string, error) {
	var p ResourcesReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if p.URI == "" {
		return "", fmt.Errorf("uri is required")
	}
	return p.URI, nil
}
