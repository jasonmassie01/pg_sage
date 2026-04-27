package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleDuplicateIndex struct{}

func (r *ruleDuplicateIndex) ID() string       { return "lint_duplicate_index" }
func (r *ruleDuplicateIndex) Name() string     { return "Duplicate Index" }
func (r *ruleDuplicateIndex) Severity() string { return "warning" }
func (r *ruleDuplicateIndex) Category() string { return "performance" }

func (r *ruleDuplicateIndex) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
WITH idx_defs AS (
    SELECT n.nspname AS schema_name,
           ct.relname AS table_name,
           ci.relname AS index_name,
           ci.oid AS index_oid,
           i.indrelid, i.indclass::text, i.indkey::text,
           pg_get_expr(i.indexprs, i.indrelid) AS exprs,
           pg_get_expr(i.indpred, i.indrelid) AS pred,
           pg_relation_size(ci.oid) AS index_size
      FROM pg_index i
      JOIN pg_class ci ON ci.oid = i.indexrelid
      JOIN pg_class ct ON ct.oid = i.indrelid
      JOIN pg_namespace n ON n.oid = ct.relnamespace
     WHERE n.nspname NOT IN (%s)
)
SELECT d1.schema_name, d1.table_name,
       d1.index_name AS dup_index,
       d2.index_name AS kept_index,
       d1.index_size
  FROM idx_defs d1
  JOIN idx_defs d2
    ON d1.indrelid = d2.indrelid
   AND d1.indclass = d2.indclass
   AND d1.indkey = d2.indkey
   AND COALESCE(d1.exprs, '') = COALESCE(d2.exprs, '')
   AND COALESCE(d1.pred, '') = COALESCE(d2.pred, '')
   AND d1.index_oid > d2.index_oid
 ORDER BY d1.index_size DESC`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleDuplicateIndex query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleDuplicateIndex) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, dupIdx, keptIdx string
		var indexSize int64
		if err := rows.Scan(&schema, &table, &dupIdx, &keptIdx, &indexSize); err != nil {
			return nil, fmt.Errorf("ruleDuplicateIndex scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    table,
			Index:    dupIdx,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Index %s.%s is a duplicate of %s (%s wasted)",
				schema, dupIdx, keptIdx, humanSize(indexSize)),
			Impact: "Duplicate indexes double write overhead and waste " +
				"disk space without any query benefit",
			Suggestion: fmt.Sprintf(
				"Drop the duplicate: DROP INDEX CONCURRENTLY %s.%s",
				schema, dupIdx),
			SQL:       fmt.Sprintf("DROP INDEX CONCURRENTLY %s.%s;", schema, dupIdx),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleDuplicateIndex rows: %w", err)
	}
	return findings, nil
}
