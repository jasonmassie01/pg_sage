package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleMissingFKIndex struct{}

func (r *ruleMissingFKIndex) ID() string       { return "lint_missing_fk_index" }
func (r *ruleMissingFKIndex) Name() string     { return "Missing FK Index" }
func (r *ruleMissingFKIndex) Severity() string { return "warning" }
func (r *ruleMissingFKIndex) Category() string { return "indexing" }

func (r *ruleMissingFKIndex) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT cn.conname       AS constraint_name,
       ns.nspname       AS schema_name,
       cl.relname       AS table_name,
       (SELECT string_agg(a.attname, ', ' ORDER BY ap.ord)
          FROM unnest(cn.conkey) WITH ORDINALITY AS ap(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = cn.conrelid
                              AND a.attnum = ap.attnum
       )                AS column_names,
       rcl.relname      AS referenced_table
  FROM pg_constraint cn
  JOIN pg_class cl  ON cl.oid = cn.conrelid
  JOIN pg_namespace ns ON ns.oid = cl.relnamespace
  JOIN pg_class rcl ON rcl.oid = cn.confrelid
 WHERE cn.contype = 'f'
   AND ns.nspname NOT IN (%s)
   AND cl.reltuples >= $1
   AND NOT EXISTS (
       SELECT 1
         FROM pg_index ix
        WHERE ix.indrelid = cn.conrelid
          AND cn.conkey = ix.indkey[1:array_length(cn.conkey, 1)]
   )
 ORDER BY ns.nspname, cl.relname`, excludeList)

	return r.exec(ctx, pool, query, opts.MinTableRows)
}

func (r *ruleMissingFKIndex) exec(
	ctx context.Context, pool *pgxpool.Pool, query string, minRows int,
) ([]Finding, error) {
	rows, err := pool.Query(ctx, query, minRows)
	if err != nil {
		return nil, fmt.Errorf("ruleMissingFKIndex: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var constraint, schema, table, cols, refTable string
		if err := rows.Scan(&constraint, &schema, &table, &cols, &refTable); err != nil {
			return nil, fmt.Errorf("ruleMissingFKIndex scan: %w", err)
		}
		suggestion := fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY ON %s.%s (%s)", schema, table, cols,
		)
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   cols,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Foreign key %s on %s.%s(%s) references %s but has no supporting index",
				constraint, schema, table, cols, refTable,
			),
			Impact:     "JOINs and ON DELETE/UPDATE CASCADE operations will require sequential scans on the referencing table",
			Suggestion: suggestion,
			SQL:        suggestion,
			FirstSeen:  now,
			LastSeen:   now,
		})
	}
	return findings, rows.Err()
}
