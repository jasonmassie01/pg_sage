package tuner

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// QueryContext holds enriched context for LLM-based hint reasoning.
type QueryContext struct {
	Candidate     candidate
	PlanJSON      string
	Symptoms      []PlanSymptom
	FallbackHints string
	Tables        []TableDetail
	System        SystemContext
}

// TableDetail holds per-table stats for LLM context.
type TableDetail struct {
	Schema     string
	Name       string
	LiveTuples int64
	DeadTuples int64
	TableBytes int64
	Columns    []ColumnInfo
	Indexes    []IndexDetail
	ColStats   []ColStatInfo
}

// ColumnInfo describes a table column.
type ColumnInfo struct {
	Name       string
	Type       string
	IsNullable bool
}

// IndexDetail describes an existing index.
type IndexDetail struct {
	Name       string
	Definition string
	Scans      int64
	IsUnique   bool
}

// ColStatInfo holds pg_stats data for a column.
type ColStatInfo struct {
	Column      string
	NDistinct   float64
	Correlation float64
}

// SystemContext holds system-level GUC values for LLM reasoning.
type SystemContext struct {
	ActiveBackends int
	MaxConnections int
	WorkMem        string
	SharedBuffers  string
	EffCacheSize   string
	MaxParallelPG  int
}

const maxContextTables = 5

func buildQueryContext(
	ctx context.Context,
	pool *pgxpool.Pool,
	c candidate,
	symptoms []PlanSymptom,
	planJSON string,
	fallbackHints string,
) QueryContext {
	qctx := QueryContext{
		Candidate:     c,
		PlanJSON:      planJSON,
		Symptoms:      symptoms,
		FallbackHints: fallbackHints,
		System:        fetchSystemContext(ctx, pool),
	}
	tables := extractTables(c.Query)
	if len(tables) > maxContextTables {
		tables = tables[:maxContextTables]
	}
	for _, t := range tables {
		td := fetchTableDetail(ctx, pool, t.schema, t.name)
		qctx.Tables = append(qctx.Tables, td)
	}
	return qctx
}

type tableRef struct {
	schema string
	name   string
}

func extractTables(query string) []tableRef {
	upper := strings.ToUpper(query)
	words := strings.Fields(upper)
	seen := make(map[string]bool)
	var refs []tableRef
	for i, w := range words {
		kw := w == "FROM" || w == "JOIN" || w == "UPDATE" || w == "INTO"
		if !kw || i+1 >= len(words) {
			continue
		}
		raw := strings.ToLower(words[i+1])
		raw = strings.Trim(raw, "\"(),;")
		if raw == "" || strings.HasPrefix(raw, "pg_") {
			continue
		}
		schema, name := "public", raw
		if idx := strings.Index(raw, "."); idx > 0 {
			schema = raw[:idx]
			name = raw[idx+1:]
		}
		if skipSchema(schema) {
			continue
		}
		key := schema + "." + name
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, tableRef{schema, name})
	}
	return refs
}

func fetchTableDetail(
	ctx context.Context,
	pool *pgxpool.Pool,
	schema, table string,
) TableDetail {
	td := TableDetail{Schema: schema, Name: table}
	_ = pool.QueryRow(ctx,
		`SELECT n_live_tup, n_dead_tup, pg_total_relation_size(c.oid)
		 FROM pg_stat_user_tables s
		 JOIN pg_class c ON c.relname = s.relname
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		   AND n.nspname = s.schemaname
		 WHERE s.schemaname = $1 AND s.relname = $2`,
		schema, table,
	).Scan(&td.LiveTuples, &td.DeadTuples, &td.TableBytes)
	td.Columns = fetchColumns(ctx, pool, schema, table)
	td.Indexes = fetchIndexes(ctx, pool, schema, table)
	td.ColStats = fetchColStats(ctx, pool, schema, table)
	return td
}

func fetchColumns(
	ctx context.Context,
	pool *pgxpool.Pool,
	schema, table string,
) []ColumnInfo {
	rows, err := pool.Query(ctx,
		`SELECT column_name, data_type, is_nullable
		 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2
		 ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []ColumnInfo
	for rows.Next() {
		var c ColumnInfo
		var nullable string
		if err := rows.Scan(&c.Name, &c.Type, &nullable); err != nil {
			continue
		}
		c.IsNullable = nullable == "YES"
		cols = append(cols, c)
	}
	return cols
}

func fetchIndexes(
	ctx context.Context,
	pool *pgxpool.Pool,
	schema, table string,
) []IndexDetail {
	rows, err := pool.Query(ctx,
		`SELECT s.indexrelname,
		        pg_get_indexdef(i.indexrelid),
		        s.idx_scan,
		        i.indisunique
		 FROM pg_stat_user_indexes s
		 JOIN pg_index i ON i.indexrelid = s.indexrelid
		 WHERE s.schemaname = $1 AND s.relname = $2`,
		schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var idxs []IndexDetail
	for rows.Next() {
		var d IndexDetail
		if err := rows.Scan(
			&d.Name, &d.Definition, &d.Scans, &d.IsUnique,
		); err != nil {
			continue
		}
		idxs = append(idxs, d)
	}
	return idxs
}

func fetchColStats(
	ctx context.Context,
	pool *pgxpool.Pool,
	schema, table string,
) []ColStatInfo {
	rows, err := pool.Query(ctx,
		`SELECT attname, n_distinct, correlation
		 FROM pg_stats
		 WHERE schemaname = $1 AND tablename = $2`,
		schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var stats []ColStatInfo
	for rows.Next() {
		var s ColStatInfo
		var corr *float64
		if err := rows.Scan(&s.Column, &s.NDistinct, &corr); err != nil {
			continue
		}
		if corr != nil {
			s.Correlation = *corr
		}
		stats = append(stats, s)
	}
	return stats
}

var systemGUCNames = []string{
	"work_mem", "shared_buffers", "effective_cache_size",
	"max_parallel_workers_per_gather", "max_connections",
}

func fetchSystemContext(
	ctx context.Context,
	pool *pgxpool.Pool,
) SystemContext {
	sc := SystemContext{}
	gucs := make(map[string]string)
	rows, err := pool.Query(ctx,
		`SELECT name, setting FROM pg_settings
		 WHERE name = ANY($1)`, systemGUCNames)
	if err != nil {
		return sc
	}
	defer rows.Close()
	for rows.Next() {
		var name, setting string
		if err := rows.Scan(&name, &setting); err != nil {
			continue
		}
		gucs[name] = setting
	}
	sc.WorkMem = gucs["work_mem"]
	sc.SharedBuffers = gucs["shared_buffers"]
	sc.EffCacheSize = gucs["effective_cache_size"]
	sc.MaxConnections = parseIntSetting(gucs["max_connections"])
	sc.MaxParallelPG = parseIntSetting(
		gucs["max_parallel_workers_per_gather"],
	)
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		 WHERE state = 'active'`,
	).Scan(&sc.ActiveBackends)
	return sc
}

func parseIntSetting(s string) int {
	var v int
	fmt.Sscanf(s, "%d", &v)
	return v
}

var skipSchemas = map[string]bool{
	"sage":               true,
	"pg_catalog":         true,
	"information_schema": true,
	"pg_toast":           true,
	"pg_temp":            true,
	"hint_plan":          true,
}

func skipSchema(schema string) bool {
	return skipSchemas[schema]
}
