package workpack

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// schema.json: the user schemas' tables and partition roots (partitions
// folded into their root), their columns, constraints and index
// definitions, bounded at MaxSchemaTables tables and MaxSchemaColumns
// columns.

// schemaTableRow is one table as read. total is how many tables the read
// counted before its bound.
type schemaTableRow struct {
	oid                uint32
	schema, name, kind string
	size               int64
	partKey            string
	partCount, total   int
	columns            []ColumnFact
	constraints        []string
	indexes            []string
}

var schemaTablesSQL = `/* pg_sage */ SELECT c.oid, n.nspname, c.relname,
       CASE c.relkind WHEN 'p' THEN 'partitioned' ELSE 'table' END,
       CASE WHEN c.relkind = 'p' THEN (SELECT COALESCE(sum(pg_total_relation_size(t.relid)), 0)
              FROM pg_partition_tree(c.oid) t WHERE t.isleaf)::int8
            ELSE pg_total_relation_size(c.oid) END,
       CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) ELSE '' END,
       (SELECT count(*) FROM pg_inherits h WHERE h.inhparent = c.oid)::int,
       count(*) OVER ()::int
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND ` + userSchemas + `
 ORDER BY n.nspname, c.relname
 LIMIT ` + fmt.Sprint(MaxSchemaTables+1)

// schemaPartsSQL lists each table's columns (kind c), constraints (k) and
// index definitions (i), in the tables' order, at most one past the bound
// of each kind.
var schemaPartsSQL = `/* pg_sage */ WITH t AS (
    SELECT u.oid, u.ord FROM unnest($1::oid[]) WITH ORDINALITY AS u(oid, ord))
(SELECT 'c', t.oid, a.attname::text, format_type(a.atttypid, a.atttypmod), a.attnotnull
   FROM t JOIN pg_attribute a ON a.attrelid = t.oid
  WHERE a.attnum > 0 AND NOT a.attisdropped
  ORDER BY t.ord, a.attnum LIMIT ` + fmt.Sprint(MaxSchemaColumns+1) + `)
UNION ALL
(SELECT 'k', t.oid, k.conname::text, pg_get_constraintdef(k.oid), false
   FROM t JOIN pg_constraint k ON k.conrelid = t.oid
  ORDER BY t.ord, k.conname LIMIT ` + fmt.Sprint(MaxSchemaColumns+1) + `)
UNION ALL
(SELECT 'i', t.oid, ci.relname::text, pg_get_indexdef(i.indexrelid), false
   FROM t JOIN pg_index i ON i.indrelid = t.oid JOIN pg_class ci ON ci.oid = i.indexrelid
  ORDER BY t.ord, ci.relname LIMIT ` + fmt.Sprint(MaxSchemaColumns+1) + `)`

func readSchema(ctx context.Context, tx pgx.Tx, _ catalogInput, cat *catalogData) error {
	rows, err := tx.Query(ctx, schemaTablesSQL)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (schemaTableRow,
		error) {
		var x schemaTableRow
		err := row.Scan(&x.oid, &x.schema, &x.name, &x.kind, &x.size, &x.partKey,
			&x.partCount, &x.total)
		return x, err
	})
	if err != nil || len(tables) == 0 {
		return err
	}
	if err := readSchemaParts(ctx, tx, tables); err != nil {
		return err
	}
	cat.tables = tables
	return nil
}

func readSchemaParts(ctx context.Context, tx pgx.Tx, tables []schemaTableRow) error {
	at := map[uint32]int{}
	oids := make([]uint32, len(tables))
	for i, t := range tables {
		at[t.oid], oids[i] = i, t.oid
	}
	rows, err := tx.Query(ctx, schemaPartsSQL, oids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, def string
		var oid uint32
		var notNull bool
		if err := rows.Scan(&kind, &oid, &name, &def, &notNull); err != nil {
			return err
		}
		t := &tables[at[oid]]
		switch kind {
		case "c":
			t.columns = append(t.columns, ColumnFact{Name: name, Type: def, NotNull: notNull})
		case "k":
			t.constraints = append(t.constraints, name+" "+def)
		case "i":
			t.indexes = append(t.indexes, def)
		}
	}
	return rows.Err()
}

// buildSchema keeps tables while both bounds hold.
func buildSchema(rows []schemaTableRow) Schema {
	out := Schema{Tables: []TableFact{}, TablesTotal: len(rows)}
	if len(rows) > 0 && rows[0].total > out.TablesTotal {
		out.TablesTotal = rows[0].total
	}
	for _, x := range rows {
		if len(out.Tables) == MaxSchemaTables ||
			out.ColumnsTotal+len(x.columns) > MaxSchemaColumns {
			out.Truncated = true
			break
		}
		out.ColumnsTotal += len(x.columns)
		out.Tables = append(out.Tables, TableFact{ID: "schema/table/" + x.schema + "." + x.name,
			Table: x.schema + "." + x.name, Kind: x.kind, SizeBytes: x.size,
			PartitionKey: x.partKey, PartitionCount: x.partCount,
			Columns: orEmptyCols(x.columns), Constraints: orEmpty(x.constraints),
			Indexes: orEmpty(x.indexes)})
	}
	return out
}

func orEmptyCols(c []ColumnFact) []ColumnFact {
	if c == nil {
		return []ColumnFact{}
	}
	return c
}
