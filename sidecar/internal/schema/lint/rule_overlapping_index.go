package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleOverlappingIndex struct{}

func (r *ruleOverlappingIndex) ID() string       { return "lint_overlapping_index" }
func (r *ruleOverlappingIndex) Name() string     { return "Overlapping Index" }
func (r *ruleOverlappingIndex) Severity() string { return "info" }
func (r *ruleOverlappingIndex) Category() string { return "performance" }

func (r *ruleOverlappingIndex) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname AS schema_name,
       ct.relname AS table_name,
       ci_short.relname AS short_index,
       ci_long.relname AS long_index,
       pg_relation_size(ci_short.oid) AS short_size
  FROM pg_index a
  JOIN pg_index b ON a.indrelid = b.indrelid
                 AND a.indexrelid <> b.indexrelid
  JOIN pg_class ci_short ON ci_short.oid = a.indexrelid
  JOIN pg_class ci_long  ON ci_long.oid  = b.indexrelid
  JOIN pg_class ct       ON ct.oid = a.indrelid
  JOIN pg_namespace n    ON n.oid = ct.relnamespace
 WHERE n.nspname NOT IN (%s)
   AND a.indexprs IS NULL AND b.indexprs IS NULL
   AND a.indpred IS NULL AND b.indpred IS NULL
   AND array_length(a.indkey, 1) < array_length(b.indkey, 1)
   AND (a.indkey::int[])[0:array_length(a.indkey, 1) - 1]
       = (b.indkey::int[])[0:array_length(a.indkey, 1) - 1]
   AND (a.indclass::oid[])[0:array_length(a.indclass, 1) - 1]
       = (b.indclass::oid[])[0:array_length(a.indclass, 1) - 1]
 ORDER BY pg_relation_size(ci_short.oid) DESC`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleOverlappingIndex query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleOverlappingIndex) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, shortIdx, longIdx string
		var shortSize int64
		if err := rows.Scan(&schema, &table, &shortIdx, &longIdx, &shortSize); err != nil {
			return nil, fmt.Errorf("ruleOverlappingIndex scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Index:    shortIdx,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Index %s.%s is a prefix of %s and likely redundant (%s)",
				schema, shortIdx, longIdx, humanSize(shortSize)),
			Impact: "The longer index can satisfy any query the shorter " +
				"index serves. The shorter index wastes disk and write I/O",
			Suggestion: fmt.Sprintf(
				"Verify with pg_stat_user_indexes, then: "+
					"DROP INDEX CONCURRENTLY %s.%s",
				schema, shortIdx),
			SQL:       fmt.Sprintf("DROP INDEX CONCURRENTLY %s.%s;", schema, shortIdx),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleOverlappingIndex rows: %w", err)
	}
	return findings, nil
}
