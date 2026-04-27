package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleVarchar255 struct{}

func (r *ruleVarchar255) ID() string       { return "lint_varchar_255" }
func (r *ruleVarchar255) Name() string     { return "varchar(255) Usage" }
func (r *ruleVarchar255) Severity() string { return "info" }
func (r *ruleVarchar255) Category() string { return "convention" }

func (r *ruleVarchar255) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.atttypmod = 259
   AND a.atttypid = 1043
   AND a.attnum > 0
   AND NOT a.attisdropped
   AND c.relkind = 'r'
   AND n.nspname NOT IN (%s)
 ORDER BY n.nspname, c.relname, a.attname`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleVarchar255 query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleVarchar255) collect(rows interface {
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
			return nil, fmt.Errorf("ruleVarchar255 scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Column %s.%s.%s uses varchar(255) — a common MySQL legacy pattern",
				schema, table, column),
			Impact: "In PostgreSQL, varchar(255) has no performance benefit " +
				"over text. The limit is arbitrary and often too short " +
				"for real-world data",
			Suggestion: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE text",
				schema, table, column),
			SQL: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE text;",
				schema, table, column),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleVarchar255 rows: %w", err)
	}
	return findings, nil
}
