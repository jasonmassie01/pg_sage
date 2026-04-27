package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleBloatedTable struct{}

func (r *ruleBloatedTable) ID() string       { return "lint_bloated_table" }
func (r *ruleBloatedTable) Name() string     { return "Bloated Table" }
func (r *ruleBloatedTable) Severity() string { return "warning" }
func (r *ruleBloatedTable) Category() string { return "performance" }

func (r *ruleBloatedTable) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, c.relpages, c.reltuples::bigint,
       COALESCE(s.last_autovacuum, s.last_vacuum) AS last_vacuum,
       CASE WHEN expected_pages > 0
            THEN ((c.relpages - expected_pages)::float / c.relpages) * 100
            ELSE 0 END AS bloat_pct,
       pg_total_relation_size(c.oid) AS total_size
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_stat_user_tables s
    ON s.schemaname = n.nspname AND s.relname = c.relname
  CROSS JOIN LATERAL (
      SELECT GREATEST(1, ceil(
          c.reltuples * COALESCE(
              (SELECT avg(avg_width) FROM pg_stats
                WHERE schemaname = n.nspname AND tablename = c.relname),
              24) / current_setting('block_size')::int
      ))::bigint AS expected_pages
  ) ep
 WHERE c.relkind = 'r'
   AND n.nspname NOT IN (%s)
   AND c.reltuples >= $1
   AND c.relpages > 0
   AND ((c.relpages - ep.expected_pages)::float / c.relpages) > 0.3
 ORDER BY bloat_pct DESC
 LIMIT 200`, excludeList)

	rows, err := pool.Query(ctx, query, opts.MinTableRows)
	if err != nil {
		return nil, fmt.Errorf("ruleBloatedTable query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleBloatedTable) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table string
		var relpages, reltuples, totalSize int64
		var lastVacuum *time.Time
		var bloatPct float64
		err := rows.Scan(
			&schema, &table, &relpages, &reltuples,
			&lastVacuum, &bloatPct, &totalSize,
		)
		if err != nil {
			return nil, fmt.Errorf("ruleBloatedTable scan: %w", err)
		}
		sev := r.Severity()
		if bloatPct > 60 {
			sev = "critical"
		}
		vacStr := "never"
		if lastVacuum != nil {
			vacStr = lastVacuum.Format("2006-01-02 15:04")
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Severity: sev,
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Table %s.%s is ~%.0f%% bloated (%s total, ~%d rows)",
				schema, table, bloatPct, humanSize(totalSize), reltuples),
			Impact: fmt.Sprintf(
				"Last vacuum: %s. Bloated tables waste disk, pollute "+
					"shared_buffers, and slow sequential scans", vacStr),
			Suggestion: fmt.Sprintf(
				"Run VACUUM FULL %s.%s (requires exclusive lock) or "+
					"use pg_repack for online compaction", schema, table),
			SQL:       fmt.Sprintf("VACUUM FULL %s.%s;", schema, table),
			TableSize: totalSize,
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleBloatedTable rows: %w", err)
	}
	return findings, nil
}
