package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleWideTable struct{}

func (r *ruleWideTable) ID() string       { return "lint_wide_table" }
func (r *ruleWideTable) Name() string     { return "Wide Table" }
func (r *ruleWideTable) Severity() string { return "info" }
func (r *ruleWideTable) Category() string { return "performance" }

func (r *ruleWideTable) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, count(*) AS col_count
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.attnum > 0
   AND NOT a.attisdropped
   AND c.relkind = 'r'
   AND n.nspname NOT IN (%s)
 GROUP BY n.nspname, c.relname
HAVING count(*) > 50
 ORDER BY col_count DESC`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleWideTable query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleWideTable) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table string
		var colCount int64
		if err := rows.Scan(&schema, &table, &colCount); err != nil {
			return nil, fmt.Errorf("ruleWideTable scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Table %s.%s has %d columns",
				schema, table, colCount),
			Impact: "Wide tables increase row width, reduce rows per page, " +
				"and force every SELECT * to transfer unnecessary data. " +
				"This degrades cache efficiency and I/O throughput",
			Suggestion: "Consider normalizing into related tables or " +
				"using JSONB for sparse/optional attributes",
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleWideTable rows: %w", err)
	}
	return findings, nil
}
