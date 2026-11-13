package plan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// swapTableSQL reads the table and the column ($1 schema, $2 table, $3
// column). A missing column leaves the attribute fields NULL.
const swapTableSQL = `
SELECT c.oid, c.relkind::text, c.relispartition,
       EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
               WHERE i.inhrelid = c.oid OR i.inhparent = c.oid),
       GREATEST(c.reltuples, 0)::bigint,
       pg_catalog.current_setting('server_version_num')::int,
       a.attnum, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
       a.attidentity <> '', a.attgenerated <> '',
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '')
FROM   pg_catalog.pg_class c
JOIN   pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attribute a
       ON a.attrelid = c.oid AND a.attname = $3 AND a.attnum > 0 AND NOT a.attisdropped
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
WHERE  n.nspname = $1 AND c.relname = $2`

// swapSequenceSQL reads the sequences the column owns ($1 table oid, $2
// attnum) and whether the column's default depends on each, the one the
// default uses first.
const swapSequenceSQL = `
SELECT sn.nspname, s.relname, pg_catalog.format_type(q.seqtypid, NULL),
       EXISTS (SELECT 1 FROM pg_catalog.pg_attrdef ad
               JOIN pg_catalog.pg_depend dd
                 ON dd.classid = 'pg_catalog.pg_attrdef'::regclass AND dd.objid = ad.oid
               WHERE ad.adrelid = $1 AND ad.adnum = $2
                 AND dd.refclassid = 'pg_catalog.pg_class'::regclass AND dd.refobjid = s.oid)
FROM   pg_catalog.pg_depend d
JOIN   pg_catalog.pg_class s ON s.oid = d.objid AND s.relkind = 'S'
JOIN   pg_catalog.pg_namespace sn ON sn.oid = s.relnamespace
JOIN   pg_catalog.pg_sequence q ON q.seqrelid = s.oid
WHERE  d.classid = 'pg_catalog.pg_class'::regclass
  AND  d.refclassid = 'pg_catalog.pg_class'::regclass
  AND  d.refobjid = $1 AND d.refobjsubid = $2 AND d.deptype = 'a'
ORDER  BY 4 DESC, 1, 2`

// swapPrimaryKeySQL reads the primary key ($1 table oid): its name, its
// columns in key order, the first column's type and the shapes the swap
// cannot rebuild (DEFERRABLE, INCLUDE columns, replica identity).
const swapPrimaryKeySQL = `
SELECT con.conname,
       ARRAY(SELECT a.attname::text
             FROM unnest(con.conkey) WITH ORDINALITY AS k(num, ord)
             JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.num
             ORDER BY k.ord),
       (SELECT pg_catalog.format_type(a.atttypid, a.atttypmod)
        FROM pg_catalog.pg_attribute a
        WHERE a.attrelid = con.conrelid AND a.attnum = con.conkey[1]),
       con.condeferrable, i.indnatts <> i.indnkeyatts, i.indisreplident
FROM   pg_catalog.pg_constraint con
JOIN   pg_catalog.pg_index i ON i.indexrelid = con.conindid
WHERE  con.conrelid = $1 AND con.contype = 'p'`

// ReadSwapFacts reads what PlanColumnSwap needs about schema.table.column
// through q, which bounds each read (catalogread.Reader). Every read is a
// catalog query; nothing is written.
func ReadSwapFacts(
	ctx context.Context, q catalogread.Querier, schema, table, column string,
) (SwapFacts, error) {
	f := SwapFacts{Schema: schema, Table: table}
	oid, attnum, err := readSwapTable(ctx, q, &f, column)
	if err != nil {
		return SwapFacts{}, err
	}
	steps := []func() error{
		func() error { return readSwapSequences(ctx, q, &f, oid, attnum) },
		func() error { return readSwapPrimaryKey(ctx, q, &f, oid) },
		func() error { return readSwapForeignKeys(ctx, q, &f, oid, attnum) },
		func() error { return readSwapDependents(ctx, q, &f, oid, attnum) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return SwapFacts{}, fmt.Errorf("read column swap facts for %s.%s.%s: %w",
				schema, table, column, err)
		}
	}
	return f, nil
}

func readSwapTable(
	ctx context.Context, q catalogread.Querier, f *SwapFacts, column string,
) (uint32, int16, error) {
	var oid uint32
	var attnum *int16
	var typ *string
	var notNull, identity, generated *bool
	err := q.QueryRow(ctx, swapTableSQL, f.Schema, f.Table, column).Scan(
		&oid, &f.RelKind, &f.IsPartition, &f.HasInheritance, &f.EstimatedRows,
		&f.ServerVersion, &attnum, &typ, &notNull, &identity, &generated, &f.Column.Default)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("column swap: table %s.%s not found", f.Schema, f.Table)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("column swap: read table %s.%s: %w", f.Schema, f.Table, err)
	}
	if attnum == nil {
		return 0, 0, fmt.Errorf("column swap: column %q not found on %s.%s",
			column, f.Schema, f.Table)
	}
	f.Column.Name, f.Column.Type, f.Column.NotNull = column, *typ, *notNull
	f.Column.Identity, f.Column.Generated = *identity, *generated
	return oid, *attnum, nil
}

// readSwapSequences reads the column's owned sequence. The swap moves
// one; any other sequence the column owns would be dropped with the old
// column, so it is recorded as a dependent, which refuses the swap.
func readSwapSequences(
	ctx context.Context, q catalogread.Querier, f *SwapFacts, oid uint32, attnum int16,
) error {
	rows, err := q.Query(ctx, swapSequenceSQL, oid, attnum)
	if err != nil {
		return fmt.Errorf("owned sequence: %w", err)
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SwapColumn, error) {
		var c SwapColumn
		err := r.Scan(&c.SequenceSchema, &c.SequenceName, &c.SequenceType, &c.DefaultIsSeq)
		return c, err
	})
	if err != nil {
		return fmt.Errorf("owned sequence: %w", err)
	}
	for i, s := range seqs {
		if i == 0 {
			c := &f.Column
			c.SequenceSchema, c.SequenceName = s.SequenceSchema, s.SequenceName
			c.SequenceType, c.DefaultIsSeq = s.SequenceType, s.DefaultIsSeq
			continue
		}
		f.Dependents = append(f.Dependents,
			"sequence "+s.SequenceSchema+"."+s.SequenceName)
	}
	return nil
}

func readSwapPrimaryKey(
	ctx context.Context, q catalogread.Querier, f *SwapFacts, oid uint32,
) error {
	var key SwapKey
	var colType *string
	err := q.QueryRow(ctx, swapPrimaryKeySQL, oid).Scan(&key.Constraint, &key.Columns, &colType,
		&key.Deferrable, &key.Include, &key.ReplicaIdentity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("primary key: %w", err)
	}
	if colType != nil {
		key.ColumnType = *colType
	}
	f.PrimaryKey = &key
	return nil
}
