package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleNullableUnique struct{}

func (r *ruleNullableUnique) ID() string       { return "lint_nullable_unique" }
func (r *ruleNullableUnique) Name() string     { return "Nullable Unique Column" }
func (r *ruleNullableUnique) Severity() string { return "info" }
func (r *ruleNullableUnique) Category() string { return "correctness" }

func (r *ruleNullableUnique) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, ct.relname, a.attname, ci.relname AS index_name
  FROM pg_index i
  JOIN pg_class ci ON ci.oid = i.indexrelid
  JOIN pg_class ct ON ct.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = ct.relnamespace
  JOIN pg_attribute a ON a.attrelid = i.indrelid
                     AND a.attnum = i.indkey[0]
 WHERE i.indisunique
   AND NOT i.indisprimary
   AND NOT a.attnotnull
   AND array_length(i.indkey, 1) = 1
   AND n.nspname NOT IN (%s)
   AND ct.relkind = 'r'
 ORDER BY n.nspname, ct.relname, a.attname`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleNullableUnique query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleNullableUnique) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, column, index string
		if err := rows.Scan(&schema, &table, &column, &index); err != nil {
			return nil, fmt.Errorf("ruleNullableUnique scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Index:    index,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Unique index %s.%s on nullable column %s allows multiple NULLs",
				schema, index, column),
			Impact: "PostgreSQL treats each NULL as distinct in unique " +
				"indexes. Multiple rows with NULL in this column " +
				"are allowed, which may violate business intent",
			Suggestion: fmt.Sprintf(
				"Add NOT NULL if NULLs are not expected, or create a "+
					"partial unique index: CREATE UNIQUE INDEX ON %s.%s (%s) "+
					"WHERE %s IS NOT NULL",
				schema, table, column, column),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleNullableUnique rows: %w", err)
	}
	return findings, nil
}
