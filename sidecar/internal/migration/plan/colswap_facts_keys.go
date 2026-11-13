package plan

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// swapReferencingSQL reads the foreign keys that reference the column
// ($1 table oid, $2 attnum), with each referencing table's shape. The
// first %s is the ON DELETE SET column-list test (PostgreSQL 15+), the
// second whether the key is enforced (PostgreSQL 18+).
const swapReferencingSQL = `
SELECT con.conname, cn.nspname, cc.relname,
       cc.relkind = 'p', cc.relispartition OR con.conparentid <> 0,
       COALESCE(array_length(con.conkey, 1), 0),
       COALESCE(a.attname::text, ''),
       COALESCE(pg_catalog.format_type(a.atttypid, a.atttypmod), ''),
       con.confupdtype::text, con.confdeltype::text, con.confmatchtype::text,
       con.condeferrable, con.condeferred, %s,
       NOT con.convalidated OR NOT %s
FROM   pg_catalog.pg_constraint con
JOIN   pg_catalog.pg_class cc ON cc.oid = con.conrelid
JOIN   pg_catalog.pg_namespace cn ON cn.oid = cc.relnamespace
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
WHERE  con.contype = 'f' AND con.confrelid = $1 AND $2 = ANY (con.confkey)
ORDER  BY cn.nspname, cc.relname, con.conname`

// swapOutgoingSQL names the foreign keys on the column itself.
const swapOutgoingSQL = `
SELECT con.conname FROM pg_catalog.pg_constraint con
WHERE  con.contype = 'f' AND con.conrelid = $1 AND $2 = ANY (con.conkey)
ORDER  BY con.conname`

// swapDependentsSQL describes every object that depends on the column
// except those the swap carries over: the default, the owned sequence,
// the primary key and its index, NOT NULL constraints (PostgreSQL 18)
// and foreign keys (read above).
const swapDependentsSQL = `
SELECT pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid)
FROM   pg_catalog.pg_depend d
WHERE  d.refclassid = 'pg_catalog.pg_class'::regclass
  AND  d.refobjid = $1 AND d.refobjsubid = $2 AND d.deptype IN ('n', 'a')
  AND  d.classid <> 'pg_catalog.pg_attrdef'::regclass
  AND  NOT (d.classid = 'pg_catalog.pg_class'::regclass AND d.objid IN (
         SELECT s.oid FROM pg_catalog.pg_class s WHERE s.relkind = 'S'
         UNION ALL
         SELECT con.conindid FROM pg_catalog.pg_constraint con
         WHERE con.conrelid = $1 AND con.contype = 'p'))
  AND  NOT (d.classid = 'pg_catalog.pg_constraint'::regclass AND d.objid IN (
         SELECT con.oid FROM pg_catalog.pg_constraint con
         WHERE (con.conrelid = $1 AND con.contype IN ('p', 'n', 'f'))
            OR (con.confrelid = $1 AND con.contype = 'f')))
ORDER  BY 1`

func readSwapForeignKeys(
	ctx context.Context, q catalogread.Querier, f *SwapFacts, oid uint32, attnum int16,
) error {
	setCols, enforced := "false", "true"
	if f.ServerVersion >= 150000 {
		setCols = "COALESCE(array_length(con.confdelsetcols, 1), 0) > 0"
	}
	if f.ServerVersion >= 180000 {
		enforced = "con.conenforced"
	}
	rows, err := q.Query(ctx, fmt.Sprintf(swapReferencingSQL, setCols, enforced), oid, attnum)
	if err != nil {
		return fmt.Errorf("referencing foreign keys: %w", err)
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SwapForeignKey, error) {
		var fk SwapForeignKey
		err := r.Scan(&fk.Constraint, &fk.Schema, &fk.Table, &fk.ChildPartitioned,
			&fk.ChildIsPartition, &fk.Columns, &fk.Column, &fk.ColumnType, &fk.OnUpdate,
			&fk.OnDelete, &fk.Match, &fk.Deferrable, &fk.Deferred, &fk.SetColumns,
			&fk.NotValidated)
		return fk, err
	})
	if err != nil {
		return fmt.Errorf("referencing foreign keys: %w", err)
	}
	f.Referencing = keys
	if f.Outgoing, err = collectNames(ctx, q, swapOutgoingSQL, oid, attnum); err != nil {
		return fmt.Errorf("foreign keys on the column: %w", err)
	}
	return nil
}

func readSwapDependents(
	ctx context.Context, q catalogread.Querier, f *SwapFacts, oid uint32, attnum int16,
) error {
	names, err := collectNames(ctx, q, swapDependentsSQL, oid, attnum)
	if err != nil {
		return fmt.Errorf("column dependents: %w", err)
	}
	f.Dependents = append(f.Dependents, names...)
	return nil
}

func collectNames(
	ctx context.Context, q catalogread.Querier, sql string, args ...any,
) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
