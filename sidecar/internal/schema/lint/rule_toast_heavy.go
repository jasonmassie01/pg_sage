package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleToastHeavy struct{}

func (r *ruleToastHeavy) ID() string       { return "lint_toast_heavy" }
func (r *ruleToastHeavy) Name() string     { return "TOAST-Heavy Table" }
func (r *ruleToastHeavy) Severity() string { return "info" }
func (r *ruleToastHeavy) Category() string { return "performance" }

func (r *ruleToastHeavy) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	query := toastHeavyQuery(schemaExcludeSQL(opts.ExcludeSchemas))
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleToastHeavy query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleToastHeavy) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table string
		var toastSize, totalSize int64
		var toastRatio float64
		if err := rows.Scan(&schema, &table, &toastSize, &totalSize, &toastRatio); err != nil {
			return nil, fmt.Errorf("ruleToastHeavy scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Table %s.%s TOAST storage is %.0f%% of total size "+
					"(%s TOAST / %s total)",
				schema, table, toastRatio*100,
				humanSize(toastSize), humanSize(totalSize)),
			Impact: "TOAST-heavy tables indicate large column values " +
				"(text, jsonb, bytea). Reads detoast on access, " +
				"slowing queries that select these columns",
			Suggestion: "Consider column-level EXTERNAL/EXTENDED storage " +
				"settings, or split large columns into a separate table " +
				"and join on demand",
			TableSize: totalSize,
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleToastHeavy rows: %w", err)
	}
	return findings, nil
}

// toastHeavyQuery ranks tables by TOAST share from relpages (as of the
// last VACUUM): heap, TOAST and index pages. It used to call
// pg_total_relation_size up to three times per table over the whole
// catalog (551 ms on lifeos, and every relation opened in the backend).
func toastHeavyQuery(excludeList string) string {
	return fmt.Sprintf(`/* pg_sage lint:toast_heavy */
WITH s AS (
  SELECT n.nspname, c.relname,
         t.relpages::int8 * current_setting('block_size')::int8 AS toast_size,
         (c.relpages::int8 + t.relpages::int8 + COALESCE((
             SELECT sum(ic.relpages)::int8 FROM pg_index i
               JOIN pg_class ic ON ic.oid = i.indexrelid
              WHERE i.indrelid IN (c.oid, t.oid)), 0))
           * current_setting('block_size')::int8 AS total_size
    FROM pg_class c
    JOIN pg_class t ON t.oid = c.reltoastrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relkind = 'r'
     AND n.nspname NOT IN (%s)
)
SELECT nspname, relname, toast_size, total_size,
       toast_size::float / total_size AS toast_ratio
  FROM s
 WHERE total_size > 0 AND toast_size::float / total_size > 0.5
 ORDER BY toast_size DESC
 LIMIT 200`, excludeList)
}
