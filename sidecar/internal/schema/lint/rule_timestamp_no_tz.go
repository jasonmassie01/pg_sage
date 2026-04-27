package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleTimestampNoTZ struct{}

func (r *ruleTimestampNoTZ) ID() string       { return "lint_timestamp_no_tz" }
func (r *ruleTimestampNoTZ) Name() string     { return "Timestamp Without Time Zone" }
func (r *ruleTimestampNoTZ) Severity() string { return "warning" }
func (r *ruleTimestampNoTZ) Category() string { return "correctness" }

func (r *ruleTimestampNoTZ) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.atttypid = 1114
   AND a.attnum > 0
   AND NOT a.attisdropped
   AND c.relkind = 'r'
   AND n.nspname NOT IN (%s)
   AND c.reltuples >= $1
 ORDER BY n.nspname, c.relname, a.attname`, excludeList)

	rows, err := pool.Query(ctx, query, opts.MinTableRows)
	if err != nil {
		return nil, fmt.Errorf("ruleTimestampNoTZ query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleTimestampNoTZ) collect(rows interface {
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
			return nil, fmt.Errorf("ruleTimestampNoTZ scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Column %s.%s.%s uses timestamp without time zone",
				schema, table, column),
			Impact: "timestamp without time zone stores no timezone info. " +
				"Applications in different timezones will interpret " +
				"values differently, causing silent data corruption",
			Suggestion: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE timestamptz "+
					"USING %s AT TIME ZONE 'UTC'",
				schema, table, column, column),
			SQL: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE timestamptz "+
					"USING %s AT TIME ZONE 'UTC';",
				schema, table, column, column),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleTimestampNoTZ rows: %w", err)
	}
	return findings, nil
}
