package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleLowCardinalityIndex struct{}

func (r *ruleLowCardinalityIndex) ID() string       { return "lint_low_cardinality_index" }
func (r *ruleLowCardinalityIndex) Name() string     { return "Low-Cardinality B-tree Index" }
func (r *ruleLowCardinalityIndex) Severity() string { return "info" }
func (r *ruleLowCardinalityIndex) Category() string { return "performance" }

func (r *ruleLowCardinalityIndex) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT n.nspname, ct.relname, a.attname,
       ci.relname AS index_name,
       ps.n_distinct,
       pg_relation_size(ci.oid) AS index_size
  FROM pg_index i
  JOIN pg_class ci ON ci.oid = i.indexrelid
  JOIN pg_class ct ON ct.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = ct.relnamespace
  JOIN pg_am am ON am.oid = ci.relam
  JOIN pg_attribute a ON a.attrelid = i.indrelid
                     AND a.attnum = i.indkey[0]
  JOIN pg_stats ps ON ps.schemaname = n.nspname
                  AND ps.tablename = ct.relname
                  AND ps.attname = a.attname
 WHERE am.amname = 'btree'
   AND NOT i.indisunique
   AND array_length(i.indkey, 1) = 1
   AND n.nspname NOT IN (%s)
   AND ct.reltuples >= $1
   AND ps.n_distinct >= 0 AND ps.n_distinct < 10
 ORDER BY index_size DESC`, excludeList)

	rows, err := pool.Query(ctx, query, opts.MinTableRows)
	if err != nil {
		return nil, fmt.Errorf("ruleLowCardinalityIndex query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleLowCardinalityIndex) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, table, column, index string
		var nDistinct float64
		var indexSize int64
		err := rows.Scan(&schema, &table, &column, &index, &nDistinct, &indexSize)
		if err != nil {
			return nil, fmt.Errorf("ruleLowCardinalityIndex scan: %w", err)
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
				"B-tree index %s.%s on column %s has only ~%.0f distinct values (%s)",
				schema, index, column, nDistinct, humanSize(indexSize)),
			Impact: "Low-cardinality B-tree indexes are often ignored by " +
				"the planner (sequential scan is cheaper) yet still " +
				"impose write overhead on every INSERT/UPDATE/DELETE",
			Suggestion: "Consider a partial index, BRIN index, or removing " +
				"the index if the planner never uses it",
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleLowCardinalityIndex rows: %w", err)
	}
	return findings, nil
}
