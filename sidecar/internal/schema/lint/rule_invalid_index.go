package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleInvalidIndex struct{}

func (r *ruleInvalidIndex) ID() string       { return "lint_invalid_index" }
func (r *ruleInvalidIndex) Name() string     { return "Invalid Index" }
func (r *ruleInvalidIndex) Severity() string { return "critical" }
func (r *ruleInvalidIndex) Category() string { return "indexing" }

// Check finds indexes marked as invalid (indisvalid = false).
func (r *ruleInvalidIndex) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
		SELECT n.nspname AS schema, ct.relname AS table_name,
		       ci.relname AS index_name, pg_relation_size(ci.oid) AS index_size
		FROM pg_index i
		JOIN pg_class ci ON ci.oid = i.indexrelid
		JOIN pg_class ct ON ct.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = ct.relnamespace
		WHERE NOT i.indisvalid
		  AND n.nspname NOT IN (%s)
		ORDER BY pg_relation_size(ci.oid) DESC`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleInvalidIndex query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleInvalidIndex) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, tableName, indexName string
		var indexSize int64
		if err := rows.Scan(&schema, &tableName, &indexName, &indexSize); err != nil {
			return nil, fmt.Errorf("ruleInvalidIndex scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    tableName,
			Index:    indexName,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Invalid index %s.%s on table %s (%s wasted)",
				schema, indexName, tableName, humanSize(indexSize)),
			Impact: "Invalid indexes consume disk space and are never used " +
				"by the query planner. Typically caused by a failed " +
				"CREATE INDEX CONCURRENTLY",
			Suggestion: fmt.Sprintf(
				"DROP INDEX CONCURRENTLY %s.%s; then recreate if needed",
				schema, indexName),
			SQL: fmt.Sprintf(
				"DROP INDEX CONCURRENTLY %s.%s;", schema, indexName),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleInvalidIndex rows: %w", err)
	}
	return findings, nil
}

// humanSize converts bytes to a human-readable string.
func humanSize(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
