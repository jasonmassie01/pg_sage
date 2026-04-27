package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleCharUsage struct{}

func (r *ruleCharUsage) ID() string       { return "lint_char_usage" }
func (r *ruleCharUsage) Name() string     { return "char(n) Usage" }
func (r *ruleCharUsage) Severity() string { return "warning" }
func (r *ruleCharUsage) Category() string { return "correctness" }

func (r *ruleCharUsage) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname,
       a.atttypmod - 4 AS char_length
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.atttypid = 1042
   AND a.attnum > 0
   AND NOT a.attisdropped
   AND c.relkind = 'r'
   AND n.nspname NOT IN (%s)
 ORDER BY n.nspname, c.relname, a.attname`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleCharUsage query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleCharUsage) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, column string
		var charLen int
		if err := rows.Scan(&schema, &table, &column, &charLen); err != nil {
			return nil, fmt.Errorf("ruleCharUsage scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Column %s.%s.%s uses character(%d)",
				schema, table, column, charLen),
			Impact: "character(n) right-pads with spaces to the declared " +
				"length. This wastes storage, causes subtle comparison " +
				"bugs, and is never faster than text or varchar",
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
		return nil, fmt.Errorf("ruleCharUsage rows: %w", err)
	}
	return findings, nil
}
