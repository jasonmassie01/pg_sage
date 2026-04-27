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
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname,
       pg_relation_size(c.reltoastrelid) AS toast_size,
       pg_total_relation_size(c.oid)     AS total_size,
       pg_relation_size(c.reltoastrelid)::float
           / NULLIF(pg_total_relation_size(c.oid), 0) AS toast_ratio
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind = 'r'
   AND c.reltoastrelid <> 0
   AND n.nspname NOT IN (%s)
   AND pg_total_relation_size(c.oid) > 0
   AND pg_relation_size(c.reltoastrelid)::float
       / pg_total_relation_size(c.oid) > 0.5
 ORDER BY toast_size DESC`, excludeList)

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
