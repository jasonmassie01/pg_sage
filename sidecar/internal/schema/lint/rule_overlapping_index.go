package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sanitize"
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
   AND a.indisvalid AND b.indisvalid
   -- Never propose dropping an index that enforces uniqueness or backs a
   -- constraint (PK, UNIQUE, EXCLUDE, FK target) -- G2-B10.
   AND NOT a.indisunique AND NOT a.indisprimary
   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = a.indexrelid)
   AND a.indexprs IS NULL AND b.indexprs IS NULL
   AND a.indpred IS NULL AND b.indpred IS NULL
   -- Compare key columns only; the short index must have no INCLUDE list.
   AND a.indnatts = a.indnkeyatts
   AND a.indnkeyatts < b.indnkeyatts
   AND (a.indkey::int2[])[0:a.indnkeyatts - 1]
       = (b.indkey::int2[])[0:a.indnkeyatts - 1]
   AND (a.indclass::oid[])[0:a.indnkeyatts - 1]
       = (b.indclass::oid[])[0:a.indnkeyatts - 1]
   AND (a.indoption::int2[])[0:a.indnkeyatts - 1]
       = (b.indoption::int2[])[0:a.indnkeyatts - 1]
   AND (a.indcollation::oid[])[0:a.indnkeyatts - 1]
       = (b.indcollation::oid[])[0:a.indnkeyatts - 1]
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
					"DROP INDEX CONCURRENTLY %s",
				sanitize.QuoteQualifiedName(schema, shortIdx)),
			SQL: fmt.Sprintf("DROP INDEX CONCURRENTLY %s;",
				sanitize.QuoteQualifiedName(schema, shortIdx)),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleOverlappingIndex rows: %w", err)
	}
	return findings, nil
}
