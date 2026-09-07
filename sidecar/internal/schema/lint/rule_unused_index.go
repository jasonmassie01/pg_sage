package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleUnusedIndex struct{}

func (r *ruleUnusedIndex) ID() string       { return "lint_unused_index" }
func (r *ruleUnusedIndex) Name() string     { return "Unused Index" }
func (r *ruleUnusedIndex) Severity() string { return "info" }
func (r *ruleUnusedIndex) Category() string { return "performance" }

func (r *ruleUnusedIndex) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT s.schemaname, s.relname, s.indexrelname,
       pg_relation_size(s.indexrelid) AS index_size,
       d.stats_reset
  FROM pg_stat_user_indexes s
  JOIN pg_index i ON i.indexrelid = s.indexrelid
  JOIN pg_stat_database d ON d.datname = current_database()
 WHERE s.idx_scan = 0
   AND NOT i.indisunique
   AND NOT i.indisprimary
   AND s.schemaname NOT IN (%s)
   AND d.stats_reset < now() - interval '7 days'
 ORDER BY pg_relation_size(s.indexrelid) DESC`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleUnusedIndex query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleUnusedIndex) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, index string
		var indexSize int64
		var statsReset time.Time
		if err := rows.Scan(&schema, &table, &index, &indexSize, &statsReset); err != nil {
			return nil, fmt.Errorf("ruleUnusedIndex scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Index:    index,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Index %s.%s on %s has zero scans since stats reset (%s wasted)",
				schema, index, table, humanSize(indexSize)),
			Impact: fmt.Sprintf(
				"Stats reset at %s. Index consumes disk and slows writes "+
					"without benefiting reads",
				statsReset.Format("2006-01-02")),
			Suggestion: "Monitor over a full business cycle before dropping. " +
				"Some indexes are only used during monthly/quarterly reports",
			SQL:       fmt.Sprintf("DROP INDEX CONCURRENTLY %s.%s;", schema, index),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleUnusedIndex rows: %w", err)
	}
	return findings, nil
}
