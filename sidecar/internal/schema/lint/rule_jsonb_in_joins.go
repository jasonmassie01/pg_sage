package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleJsonbInJoins struct{}

func (r *ruleJsonbInJoins) ID() string       { return "lint_jsonb_in_joins" }
func (r *ruleJsonbInJoins) Name() string     { return "JSONB Without GIN Index" }
func (r *ruleJsonbInJoins) Severity() string { return "warning" }
func (r *ruleJsonbInJoins) Category() string { return "performance" }

func (r *ruleJsonbInJoins) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.atttypid = 3802
   AND a.attnum > 0
   AND NOT a.attisdropped
   AND c.relkind = 'r'
   AND n.nspname NOT IN (%s)
   AND c.reltuples >= $1
   AND NOT EXISTS (
       SELECT 1
         FROM pg_index i
         JOIN pg_class ci ON ci.oid = i.indexrelid
         JOIN pg_am am ON am.oid = ci.relam
        WHERE i.indrelid = c.oid
          AND am.amname IN ('gin', 'gist')
          AND (
              a.attnum = ANY(i.indkey)
              OR pg_get_expr(i.indexprs, i.indrelid) LIKE '%%' || a.attname || '%%'
          )
   )
 ORDER BY c.reltuples DESC`, excludeList)

	rows, err := pool.Query(ctx, query, opts.MinTableRows)
	if err != nil {
		return nil, fmt.Errorf("ruleJsonbInJoins query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleJsonbInJoins) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, column string
		if err := rows.Scan(&schema, &table, &column); err != nil {
			return nil, fmt.Errorf("ruleJsonbInJoins scan: %w", err)
		}
		suggestion := fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY ON %s.%s USING gin (%s jsonb_path_ops)",
			schema, table, column)
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"JSONB column %s.%s.%s has no GIN/GiST index",
				schema, table, column),
			Impact: "Queries filtering on JSONB columns without a GIN " +
				"index require sequential scans. Write overhead " +
				"from a GIN index is usually acceptable",
			Suggestion: suggestion,
			SQL:        suggestion + ";",
			FirstSeen:  now,
			LastSeen:   now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleJsonbInJoins rows: %w", err)
	}
	return findings, nil
}
