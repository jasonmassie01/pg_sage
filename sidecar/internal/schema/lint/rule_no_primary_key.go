package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleNoPrimaryKey struct{}

func (r *ruleNoPrimaryKey) ID() string       { return "lint_no_primary_key" }
func (r *ruleNoPrimaryKey) Name() string     { return "No Primary Key" }
func (r *ruleNoPrimaryKey) Severity() string { return "warning" }
func (r *ruleNoPrimaryKey) Category() string { return "schema_design" }

// Check finds tables that lack a primary key constraint.
func (r *ruleNoPrimaryKey) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
		SELECT n.nspname, c.relname, c.reltuples::bigint AS est_rows
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_constraint pk ON pk.conrelid = c.oid AND pk.contype = 'p'
		WHERE c.relkind = 'r'
		  AND n.nspname NOT IN (%s)
		  AND c.reltuples >= $1
		  AND pk.oid IS NULL
		ORDER BY c.reltuples DESC`, excludeList)

	rows, err := pool.Query(ctx, query, opts.MinTableRows)
	if err != nil {
		return nil, fmt.Errorf("ruleNoPrimaryKey query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleNoPrimaryKey) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table string
		var estRows int64
		if err := rows.Scan(&schema, &table, &estRows); err != nil {
			return nil, fmt.Errorf("ruleNoPrimaryKey scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Table %s.%s has no primary key (~%d rows)", schema, table, estRows),
			Impact: "Tables without primary keys cannot use logical replication " +
				"and make UPDATE/DELETE operations inefficient " +
				"(sequential scan to identify rows)",
			Suggestion: "Add a primary key column or promote an existing " +
				"unique NOT NULL column",
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleNoPrimaryKey rows: %w", err)
	}
	return findings, nil
}
