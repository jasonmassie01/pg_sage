package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleSerialUsage struct{}

func (r *ruleSerialUsage) ID() string       { return "lint_serial_usage" }
func (r *ruleSerialUsage) Name() string     { return "Legacy Serial Column" }
func (r *ruleSerialUsage) Severity() string { return "info" }
func (r *ruleSerialUsage) Category() string { return "convention" }

func (r *ruleSerialUsage) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.attnum > 0
   AND NOT a.attisdropped
   AND c.relkind = 'r'
   AND n.nspname NOT IN (%s)
   AND pg_get_serial_sequence(
       quote_ident(n.nspname) || '.' || quote_ident(c.relname),
       a.attname) IS NOT NULL
   AND a.attidentity = ''
 ORDER BY n.nspname, c.relname, a.attname`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleSerialUsage query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleSerialUsage) collect(rows interface {
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
			return nil, fmt.Errorf("ruleSerialUsage scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Column %s.%s.%s uses legacy serial type instead of identity",
				schema, table, column),
			Impact: "Serial columns use a separate sequence object with " +
				"looser ownership semantics. Identity columns (PG 10+) " +
				"are the SQL-standard replacement",
			Suggestion: "Migrate to GENERATED ALWAYS AS IDENTITY. " +
				"See https://wiki.postgresql.org/wiki/Don%%27t_Do_This" +
				"#Don.27t_use_serial",
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleSerialUsage rows: %w", err)
	}
	return findings, nil
}
