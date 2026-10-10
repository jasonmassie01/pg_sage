package autonomy

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// The structural scan's statements. A pass reads them in one repeatable
// read, read-only transaction, so the table list, the text types and the
// column summaries are one snapshot of the catalog.
//
// structuralTablesSQL lists the user tables (temporary tables are another
// session's scratch space and are left out) with their pg_class row
// version: (xmin, ctid) names one tuple version, so any update of the row
// (CREATE, ADD COLUMN, a rewriting ALTER COLUMN TYPE, RENAME, SET SCHEMA)
// changes it, freezing does not (xmin reads the raw transaction id).
// Schema and table names are read fresh on every pass, so a schema
// rename needs no column work.
//
// structuralVersionsSQL fingerprints each table's column rows the same
// way: column DDL that leaves pg_class alone (RENAME COLUMN, DROP COLUMN,
// ALTER COLUMN TYPE without a rewrite) updates a pg_attribute row.
//
// structuralColumnsSQL summarizes each listed table's columns once: how
// many, how many text (a type named text or varchar, given as oids), and
// the text columns named like a number. A window over every column of
// every table spilled to disk at 5,000 relations (perf gate, 111 ms). On
// a full pass it reads every column of every user table: its tag names it
// to the performance gate, which judges its mean against its own ceiling.
const (
	structuralTablesSQL = `/* pg_sage structural:tables */
SELECT tbl.oid, ns.nspname::text, tbl.relname::text,
       tbl.xmin::text || '/' || tbl.ctid::text
FROM pg_catalog.pg_class tbl
JOIN pg_catalog.pg_namespace ns ON ns.oid=tbl.relnamespace
WHERE tbl.relkind IN ('r','p') AND tbl.relpersistence <> 't'
  AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')`

	structuralTextTypesSQL = `/* pg_sage structural:text_types */
SELECT COALESCE(array_agg(oid ORDER BY oid), '{}')
FROM pg_catalog.pg_type WHERE typname IN ('text','varchar')`

	structuralVersionsSQL = `/* pg_sage structural:versions */
SELECT att.attrelid,
       bit_xor(hashtextextended(att.xmin::text || '/' || att.ctid::text, 0))
FROM pg_catalog.pg_attribute att
WHERE att.attrelid = ANY($1::oid[]) AND att.attnum>0
GROUP BY att.attrelid`

	structuralColumnsSQL = `/* pg_sage structural:columns */
SELECT att.attrelid,
       count(*) FILTER (WHERE NOT att.attisdropped),
       count(*) FILTER (WHERE NOT att.attisdropped AND att.atttypid = ANY($2::oid[])),
       COALESCE(array_agg(att.attname::text) FILTER (WHERE NOT att.attisdropped
         AND att.atttypid = ANY($2::oid[])
         AND (att.attname='count_text' OR att.attname ~ '(_id|_count|_number)$')), '{}'),
       bit_xor(hashtextextended(att.xmin::text || '/' || att.ctid::text, 0))
FROM pg_catalog.pg_attribute att
WHERE att.attrelid = ANY($1::oid[]) AND att.attnum>0
GROUP BY att.attrelid`
)

// structuralTable is one user table's column summary: its names and row
// versions when it was summarized, its column count, text column count
// and the text columns named like a number.
type structuralTable struct {
	schema, name   string
	row            string
	columnsVersion int64
	columns        int64
	textColumns    int64
	tightening     []string
}

// structuralSnapshot is one pass's catalog reading.
type structuralSnapshot struct {
	tables    map[uint32]structuralTable
	textTypes []uint32
}

// readStructuralSnapshot runs one pass: it lists the tables and the text
// types, then summarizes the columns of the tables that are new or whose
// rows changed since prev (every table on a full pass, or when the text
// types changed); a verified pass also compares every table's column
// versions. Tables gone from the catalog are dropped.
func (d postgresSchemaDetector) readStructuralSnapshot(ctx context.Context,
	pass structuralPass, prev structuralSnapshot) (structuralSnapshot, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return structuralSnapshot{}, fmt.Errorf("begin the structural catalog snapshot: %w",
			err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only: nothing to keep
	next := structuralSnapshot{}
	if next.tables, err = listStructuralTables(ctx, tx); err != nil {
		return structuralSnapshot{}, err
	}
	if err := tx.QueryRow(ctx, structuralTextTypesSQL).Scan(&next.textTypes); err != nil {
		return structuralSnapshot{}, fmt.Errorf("read text types for the structural scan: %w",
			err)
	}
	if !slices.Equal(next.textTypes, prev.textTypes) {
		pass = structuralPassFull
	}
	stale, kept := splitStale(next.tables, prev.tables, pass)
	if pass == structuralPassVerified && len(kept) > 0 {
		moved, err := changedColumns(ctx, tx, kept, prev.tables)
		if err != nil {
			return structuralSnapshot{}, err
		}
		stale = append(stale, moved...)
	}
	if err := summarizeColumns(ctx, tx, stale, next); err != nil {
		return structuralSnapshot{}, err
	}
	return next, nil
}

func listStructuralTables(ctx context.Context, tx pgx.Tx) (map[uint32]structuralTable,
	error) {
	rows, err := tx.Query(ctx, structuralTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("list tables for the structural scan: %w", err)
	}
	defer rows.Close()
	tables := map[uint32]structuralTable{}
	for rows.Next() {
		var oid uint32
		var t structuralTable
		if err := rows.Scan(&oid, &t.schema, &t.name, &t.row); err != nil {
			return nil, fmt.Errorf("list tables for the structural scan: %w", err)
		}
		tables[oid] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables for the structural scan: %w", err)
	}
	return tables, nil
}

// splitStale fills the listed tables from prev where their pg_class row
// is unchanged (keeping the fresh names) and returns the oids to
// summarize again (new, changed, or every table on a full pass) and the
// oids kept.
func splitStale(listed, prev map[uint32]structuralTable,
	pass structuralPass) (stale, kept []uint32) {
	for oid, t := range listed {
		old, ok := prev[oid]
		if pass == structuralPassFull || !ok || old.row != t.row {
			stale = append(stale, oid)
			continue
		}
		old.schema, old.name = t.schema, t.name
		listed[oid] = old
		kept = append(kept, oid)
	}
	return stale, kept
}

// changedColumns returns the kept tables whose column rows changed.
func changedColumns(ctx context.Context, tx pgx.Tx, kept []uint32,
	prev map[uint32]structuralTable) ([]uint32, error) {
	rows, err := tx.Query(ctx, structuralVersionsSQL, kept)
	if err != nil {
		return nil, fmt.Errorf("read column versions for the structural scan: %w", err)
	}
	defer rows.Close()
	seen := make(map[uint32]bool, len(kept))
	var changed []uint32
	for rows.Next() {
		var oid uint32
		var version int64
		if err := rows.Scan(&oid, &version); err != nil {
			return nil, fmt.Errorf("read column versions for the structural scan: %w", err)
		}
		seen[oid] = true
		if version != prev[oid].columnsVersion {
			changed = append(changed, oid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read column versions for the structural scan: %w", err)
	}
	for _, oid := range kept { // a table without column rows has version 0
		if !seen[oid] && prev[oid].columnsVersion != 0 {
			changed = append(changed, oid)
		}
	}
	return changed, nil
}

// summarizeColumns summarizes the stale tables' columns into next. A
// table without columns gets an empty summary.
func summarizeColumns(ctx context.Context, tx pgx.Tx, stale []uint32,
	next structuralSnapshot) error {
	if len(stale) == 0 {
		return nil
	}
	for _, oid := range stale {
		t := next.tables[oid]
		next.tables[oid] = structuralTable{schema: t.schema, name: t.name, row: t.row}
	}
	rows, err := tx.Query(ctx, structuralColumnsSQL, stale, next.textTypes)
	if err != nil {
		return fmt.Errorf("aggregate table columns for the structural scan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var c structuralTable
		if err := rows.Scan(&oid, &c.columns, &c.textColumns, &c.tightening,
			&c.columnsVersion); err != nil {
			return fmt.Errorf("aggregate table columns for the structural scan: %w", err)
		}
		t := next.tables[oid]
		c.schema, c.name, c.row = t.schema, t.name, t.row
		next.tables[oid] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("aggregate table columns for the structural scan: %w", err)
	}
	return nil
}

// structuralInvariants is the answer the summaries give, ordered like the
// full scan: schema, table, kind, column (names compare bytewise).
func structuralInvariants(tables map[uint32]structuralTable) []schemaguard.Invariant {
	result := make([]schemaguard.Invariant, 0)
	for _, t := range tables {
		if t.columns >= 3 && t.textColumns == t.columns {
			result = append(result, schemaguard.Invariant{
				Kind: schemaguard.InvariantEverythingText, Schema: t.schema, Table: t.name})
		}
		for _, column := range t.tightening {
			result = append(result, schemaguard.Invariant{
				Kind: schemaguard.InvariantTypeTightening, Schema: t.schema, Table: t.name,
				Subject:     column,
				ProposedSQL: typeTighteningProposal(t.schema, t.name, column),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Schema != b.Schema {
			return a.Schema < b.Schema
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Subject < b.Subject
	})
	return result
}
