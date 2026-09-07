package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleIntPK struct{}

func (r *ruleIntPK) ID() string       { return "lint_int_pk" }
func (r *ruleIntPK) Name() string     { return "Integer Primary Key" }
func (r *ruleIntPK) Severity() string { return "warning" }
func (r *ruleIntPK) Category() string { return "convention" }

func (r *ruleIntPK) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, ct.relname, a.attname,
       ct.reltuples::bigint AS est_rows
  FROM pg_constraint pk
  JOIN pg_class ct ON ct.oid = pk.conrelid
  JOIN pg_namespace n ON n.oid = ct.relnamespace
  JOIN pg_attribute a ON a.attrelid = pk.conrelid
                     AND a.attnum = pk.conkey[1]
 WHERE pk.contype = 'p'
   AND array_length(pk.conkey, 1) = 1
   AND a.atttypid = 23
   AND ct.reltuples > 1000000
   AND n.nspname NOT IN (%s)
 ORDER BY ct.reltuples DESC`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleIntPK query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleIntPK) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, column string
		var estRows int64
		if err := rows.Scan(&schema, &table, &column, &estRows); err != nil {
			return nil, fmt.Errorf("ruleIntPK scan: %w", err)
		}
		pctUsed := float64(estRows) / 2_147_483_647 * 100
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Column:   column,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Table %s.%s uses int4 PK with ~%d rows (%.1f%% of int4 max)",
				schema, table, estRows, pctUsed),
			Impact: "int4 max is 2,147,483,647. Tables over 1M rows with " +
				"int4 PKs risk overflow as the table grows. Migration " +
				"to bigint is costly at scale",
			Suggestion: fmt.Sprintf(
				"Migrate to bigint early: ALTER TABLE %s.%s "+
					"ALTER COLUMN %s TYPE bigint",
				schema, table, column),
			SQL: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE bigint;",
				schema, table, column),
			TableSize: estRows,
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleIntPK rows: %w", err)
	}
	return findings, nil
}
