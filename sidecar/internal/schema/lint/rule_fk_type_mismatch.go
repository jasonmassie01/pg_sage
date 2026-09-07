package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleFKTypeMismatch struct{}

func (r *ruleFKTypeMismatch) ID() string       { return "lint_fk_type_mismatch" }
func (r *ruleFKTypeMismatch) Name() string     { return "FK Type Mismatch" }
func (r *ruleFKTypeMismatch) Severity() string { return "warning" }
func (r *ruleFKTypeMismatch) Category() string { return "correctness" }

func (r *ruleFKTypeMismatch) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT ns.nspname      AS fk_schema,
       cl.relname      AS fk_table,
       a_fk.attname    AS fk_column,
       format_type(a_fk.atttypid, a_fk.atttypmod) AS fk_type,
       rns.nspname     AS ref_schema,
       rcl.relname     AS ref_table,
       a_pk.attname    AS ref_column,
       format_type(a_pk.atttypid, a_pk.atttypmod) AS ref_type
  FROM pg_constraint cn
  JOIN pg_class cl  ON cl.oid  = cn.conrelid
  JOIN pg_namespace ns ON ns.oid = cl.relnamespace
  JOIN pg_class rcl ON rcl.oid = cn.confrelid
  JOIN pg_namespace rns ON rns.oid = rcl.relnamespace
  JOIN pg_attribute a_fk ON a_fk.attrelid = cn.conrelid
                        AND a_fk.attnum = cn.conkey[1]
  JOIN pg_attribute a_pk ON a_pk.attrelid = cn.confrelid
                        AND a_pk.attnum = cn.confkey[1]
 WHERE cn.contype = 'f'
   AND array_length(cn.conkey, 1) = 1
   AND ns.nspname NOT IN (%s)
   AND a_fk.atttypid <> a_pk.atttypid
 ORDER BY ns.nspname, cl.relname`, excludeList)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleFKTypeMismatch query: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

func (r *ruleFKTypeMismatch) collect(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Finding, error) {
	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var fkSchema, fkTable, fkCol, fkType string
		var refSchema, refTable, refCol, refType string
		err := rows.Scan(
			&fkSchema, &fkTable, &fkCol, &fkType,
			&refSchema, &refTable, &refCol, &refType,
		)
		if err != nil {
			return nil, fmt.Errorf("ruleFKTypeMismatch scan: %w", err)
		}
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   fkSchema,
			Table:    fkTable,
			Column:   fkCol,
			Severity: r.Severity(),
			Category: r.Category(),
			Description: fmt.Sprintf(
				"FK %s.%s.%s (%s) references %s.%s.%s (%s) — type mismatch",
				fkSchema, fkTable, fkCol, fkType,
				refSchema, refTable, refCol, refType),
			Impact: "Type mismatches force implicit casts during JOINs, " +
				"preventing index usage and degrading query performance",
			Suggestion: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s",
				fkSchema, fkTable, fkCol, refType),
			SQL: fmt.Sprintf(
				"ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s;",
				fkSchema, fkTable, fkCol, refType),
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ruleFKTypeMismatch rows: %w", err)
	}
	return findings, nil
}
