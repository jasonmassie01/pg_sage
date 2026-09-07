package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleTxidAge struct{}

func (r *ruleTxidAge) ID() string       { return "lint_txid_age" }
func (r *ruleTxidAge) Name() string     { return "Transaction ID Age" }
func (r *ruleTxidAge) Severity() string { return "critical" }
func (r *ruleTxidAge) Category() string { return "maintenance" }

// Check finds tables whose transaction ID age approaches wraparound.
func (r *ruleTxidAge) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, age(c.relfrozenxid) AS xid_age,
       c.reltuples::bigint AS est_rows
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 't')
  AND n.nspname NOT IN (%s)
  AND age(c.relfrozenxid) > 500000000
ORDER BY age(c.relfrozenxid) DESC
LIMIT 100`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleTxidAge query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleTxidAge) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table string
		var xidAge, estRows int64
		if err := rows.Scan(&schema, &table, &xidAge, &estRows); err != nil {
			return nil, fmt.Errorf("ruleTxidAge scan: %w", err)
		}
		sev := "warning"
		if xidAge >= 1_000_000_000 {
			sev = "critical"
		}
		suggestion := fmt.Sprintf("VACUUM FREEZE %s.%s;", schema, table)
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Severity: sev,
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Table %s.%s has transaction ID age %s (approaching wraparound)",
				schema, table, formatAge(xidAge)),
			Impact: "PostgreSQL forces a shutdown when XID age reaches " +
				"2 billion to prevent data corruption. " +
				"Autovacuum may not be keeping up.",
			Suggestion: suggestion,
			SQL:        suggestion,
			TableSize:  estRows,
			FirstSeen:  now,
			LastSeen:   now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleTxidAge rows: %w", err)
	}
	return findings, nil
}

// formatAge formats a large integer with a human-readable suffix.
func formatAge(age int64) string {
	switch {
	case age >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(age)/1_000_000_000)
	case age >= 1_000_000:
		return fmt.Sprintf("%dM", age/1_000_000)
	default:
		return fmt.Sprintf("%d", age)
	}
}
