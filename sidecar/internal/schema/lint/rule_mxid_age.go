package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleMxidAge struct{}

func (r *ruleMxidAge) ID() string       { return "lint_mxid_age" }
func (r *ruleMxidAge) Name() string     { return "MultiXact ID Age" }
func (r *ruleMxidAge) Severity() string { return "critical" }
func (r *ruleMxidAge) Category() string { return "maintenance" }

// Check finds tables whose MultiXact ID age approaches wraparound.
func (r *ruleMxidAge) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, age(c.relminmxid) AS mxid_age,
       c.reltuples::bigint AS est_rows
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 't')
  AND n.nspname NOT IN (%s)
  AND age(c.relminmxid) > 500000000
ORDER BY age(c.relminmxid) DESC
LIMIT 100`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleMxidAge query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleMxidAge) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table string
		var mxidAge, estRows int64
		if err := rows.Scan(&schema, &table, &mxidAge, &estRows); err != nil {
			return nil, fmt.Errorf("ruleMxidAge scan: %w", err)
		}
		sev := "warning"
		if mxidAge >= 1_000_000_000 {
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
				"Table %s.%s has MultiXact ID age %s (approaching wraparound)",
				schema, table, formatMxidAge(mxidAge)),
			Impact: "MultiXact ID wraparound triggers the same protective " +
				"shutdown as XID wraparound. Common when many " +
				"concurrent row locks are held.",
			Suggestion: suggestion,
			SQL:        suggestion,
			TableSize:  estRows,
			FirstSeen:  now,
			LastSeen:   now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleMxidAge rows: %w", err)
	}
	return findings, nil
}

// formatMxidAge formats a large integer with a human-readable suffix.
func formatMxidAge(age int64) string {
	switch {
	case age >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(age)/1_000_000_000)
	case age >= 1_000_000:
		return fmt.Sprintf("%dM", age/1_000_000)
	default:
		return fmt.Sprintf("%d", age)
	}
}
